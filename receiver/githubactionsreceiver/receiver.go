// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package githubactionsreceiver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v60/github"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/receiver"
	"go.opentelemetry.io/collector/receiver/receiverhelper"
	"go.uber.org/zap"
)

var errMissingEndpoint = errors.New("missing a receiver endpoint")

type githubActionsReceiver struct {
	logsConsumer   consumer.Logs
	tracesConsumer consumer.Traces
	config         *Config
	server         *http.Server
	shutdownWG     sync.WaitGroup
	createSettings receiver.CreateSettings
	logger         *zap.Logger
	obsrecv        *receiverhelper.ObsReport
	ghClient       *github.Client
}

func newReceiver(
	params receiver.CreateSettings,
	config *Config,
) (*githubActionsReceiver, error) {
	if config.Endpoint == "" {
		return nil, errMissingEndpoint
	}

	transport := "http"
	if config.TLSSetting != nil {
		transport = "https"
	}

	obsrecv, err := receiverhelper.NewObsReport(receiverhelper.ObsReportSettings{
		ReceiverID:             params.ID,
		Transport:              transport,
		ReceiverCreateSettings: params,
	})

	if err != nil {
		return nil, err
	}

	var ghClient *github.Client
	var httpClient *http.Client
	if config.GitHubAPIConfig.Auth.AppID != 0 && config.GitHubAPIConfig.Auth.InstallationID != 0 && config.GitHubAPIConfig.Auth.PrivateKeyPath != "" {
		itr, err := ghinstallation.NewKeyFromFile(http.DefaultTransport, config.GitHubAPIConfig.Auth.AppID, config.GitHubAPIConfig.Auth.InstallationID, config.GitHubAPIConfig.Auth.PrivateKeyPath)
		if err != nil {
			return nil, err
		}

		httpClient = &http.Client{Transport: itr}
	}
	ghClient = github.NewClient(httpClient)

	if config.GitHubAPIConfig.Auth.Token != "" {
		ghClient = ghClient.WithAuthToken(config.GitHubAPIConfig.Auth.Token)
	}

	if config.GitHubAPIConfig.BaseURL != "" && config.GitHubAPIConfig.UploadURL != "" {
		ghClient, err = ghClient.WithEnterpriseURLs(config.GitHubAPIConfig.BaseURL, config.GitHubAPIConfig.UploadURL)
		if err != nil {
			return nil, err
		}
	}

	gar := &githubActionsReceiver{
		config:         config,
		createSettings: params,
		logger:         params.Logger,
		obsrecv:        obsrecv,
		ghClient:       ghClient,
	}

	return gar, nil
}

// newLogsReceiver creates a trace receiver based on provided config.
func newTracesReceiver(
	_ context.Context,
	set receiver.CreateSettings,
	cfg component.Config,
	consumer consumer.Traces,
) (receiver.Traces, error) {
	rCfg := cfg.(*Config)
	var err error

	if consumer == nil {
		return nil, component.ErrNilNextConsumer
	}

	r := receivers.GetOrAdd(cfg, func() component.Component {
		var rcv component.Component
		rcv, err = newReceiver(set, rCfg)
		return rcv
	})
	if err != nil {
		return nil, err
	}

	r.Unwrap().(*githubActionsReceiver).tracesConsumer = consumer

	return r, nil
}

// newLogsReceiver creates a logs receiver based on provided config.
func newLogsReceiver(
	_ context.Context,
	set receiver.CreateSettings,
	cfg component.Config,
	consumer consumer.Logs,
) (receiver.Logs, error) {
	rCfg := cfg.(*Config)
	var err error

	if consumer == nil {
		return nil, component.ErrNilNextConsumer
	}

	r := receivers.GetOrAdd(cfg, func() component.Component {
		var rcv component.Component
		rcv, err = newReceiver(set, rCfg)
		return rcv
	})
	if err != nil {
		return nil, err
	}

	r.Unwrap().(*githubActionsReceiver).logsConsumer = consumer

	return r, nil
}

func (gar *githubActionsReceiver) Start(ctx context.Context, host component.Host) error {
	endpoint := fmt.Sprintf("%s%s", gar.config.Endpoint, gar.config.Path)
	gar.logger.Info("Starting GithubActions server", zap.String("endpoint", endpoint))
	gar.server = &http.Server{
		Addr:              gar.config.ServerConfig.Endpoint,
		Handler:           gar,
		ReadHeaderTimeout: 20 * time.Second,
	}

	gar.shutdownWG.Add(1)
	go func() {
		defer gar.shutdownWG.Done()

		if errHTTP := gar.server.ListenAndServe(); !errors.Is(errHTTP, http.ErrServerClosed) && errHTTP != nil {
			gar.createSettings.TelemetrySettings.Logger.Error("Server closed with error", zap.Error(errHTTP))
		}
	}()

	return nil
}

func (gar *githubActionsReceiver) Shutdown(ctx context.Context) error {
	var err error
	if gar.server != nil {
		err = gar.server.Close()
	}
	gar.shutdownWG.Wait()
	return err
}

func (gar *githubActionsReceiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if r.URL.Path != gar.config.Path {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	payload, err := github.ValidatePayload(r, []byte(gar.config.Secret))
	if err != nil {
		gar.logger.Error("Failed to validate payload", zap.Error(err))
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	event, err := github.ParseWebHook(github.WebHookType(r), payload)
	if err != nil {
		gar.logger.Error("Failed to parse webhook", zap.Error(err))
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	gar.logger.Debug("Received request", zap.Any("payload", event))

	traceErr := false
	// if a trace consumer is set, process the event into traces
	if gar.tracesConsumer != nil {
		td, err := eventToTraces(event, gar.config, gar.logger.Named("eventToTraces"))
		if err != nil {
			traceErr = true
			gar.logger.Error("Failed to process traces", zap.Error(err))
		}

		if td != nil {
			// Pass the traces to the nextConsumer
			consumerErr := gar.tracesConsumer.ConsumeTraces(ctx, *td)
			if consumerErr != nil {
				traceErr = true
				gar.logger.Error("Failed to consume traces", zap.Error(consumerErr))
			}
		}
	}

	// if a log consumer is set, process the event into logs
	if gar.logsConsumer != nil {
		if gar.ghClient == nil {
			gar.logger.Error("GitHub token not provided, but logs consumer is set. Logs will not be processed. Please provide a GitHub token.")
		} else {
			withTraceInfo := gar.tracesConsumer != nil && traceErr == false

			ld, err := eventToLogs(event, gar.config, gar.ghClient, gar.logger.Named("eventToLogs"), withTraceInfo)
			if err != nil {
				gar.logger.Error("Failed to process logs", zap.Error(err))
			}

			if ld != nil {

				consumerErr := gar.logsConsumer.ConsumeLogs(ctx, *ld)
				if consumerErr != nil {
					gar.logger.Error("Failed to consume logs", zap.Error(consumerErr))
				}
			}
		}

	}

	w.WriteHeader(http.StatusAccepted)
}
