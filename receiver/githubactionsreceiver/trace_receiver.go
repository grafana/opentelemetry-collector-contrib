// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package githubactionsreceiver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/google/go-github/v60/github"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/receiver"
	"go.opentelemetry.io/collector/receiver/receiverhelper"
	"go.uber.org/zap"
)

var errMissingEndpoint = errors.New("missing a receiver endpoint")

type githubActionsReceiver struct {
	nextConsumer   consumer.Traces
	config         *Config
	server         *http.Server
	shutdownWG     sync.WaitGroup
	createSettings receiver.CreateSettings
	logger         *zap.Logger
	obsrecv        *receiverhelper.ObsReport
	ghClient       *github.Client
}

func newTracesReceiver(
	params receiver.CreateSettings,
	config *Config,
	nextConsumer consumer.Traces,
) (*githubActionsReceiver, error) {
	if nextConsumer == nil {
		return nil, component.ErrNilNextConsumer
	}

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

	client := github.NewClient(nil)

	gar := &githubActionsReceiver{
		nextConsumer:   nextConsumer,
		config:         config,
		createSettings: params,
		logger:         params.Logger,
		obsrecv:        obsrecv,
		ghClient:       client,
	}

	return gar, nil
}

func (gar *githubActionsReceiver) Start(ctx context.Context, host component.Host) error {
	endpoint := fmt.Sprintf("%s%s", gar.config.Endpoint, gar.config.Path)
	gar.logger.Info("Starting GithubActions server", zap.String("endpoint", endpoint))
	gar.server = &http.Server{
		Addr:    gar.config.ServerConfig.Endpoint,
		Handler: gar,
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
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	event, err := github.ParseWebHook(github.WebHookType(r), payload)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	gar.logger.Debug("Received request", zap.Any("payload", event))

	td, err := eventToTraces(event, gar.config, gar.logger)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	gar.logger.Debug("Unmarshaled spans", zap.Int("#spans", td.SpanCount()))

	// Pass the traces to the nextConsumer
	consumerErr := gar.nextConsumer.ConsumeTraces(ctx, td)
	if consumerErr != nil {
		gar.logger.Error("Failed to process traces", zap.Error(consumerErr))
		http.Error(w, "Failed to process traces", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusAccepted)
}
