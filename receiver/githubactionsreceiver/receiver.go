// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package githubactionsreceiver

import (
	"archive/zip"
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/go-github/v58/github"
	"go.opentelemetry.io/collector/component"

	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/receiver"
	"go.opentelemetry.io/collector/receiver/receiverhelper"
	"go.uber.org/zap"
)

type githubActionsReceiver struct {
	config         *Config
	server         *http.Server
	shutdownWG     sync.WaitGroup
	createSettings receiver.CreateSettings
	logger         *zap.Logger
	obsrecv        *receiverhelper.ObsReport
	ghClient       *github.Client

	logsConsumer   consumer.Logs
	tracesConsumer consumer.Traces
}

func processWorflowJobEvent(event *github.WorkflowJobEvent, config *Config, logger *zap.Logger) (ptrace.Traces, error) {
	traces := ptrace.NewTraces()
	resourceSpans := traces.ResourceSpans().AppendEmpty()
	scopeSpans := resourceSpans.ScopeSpans().AppendEmpty()

	logger.Info("Processing WorkflowJobEvent", zap.String("job_name", *event.WorkflowJob.Name), zap.String("repo", *event.Repo.FullName))
	jobResource := resourceSpans.Resource()
	createResourceAttributes(jobResource, event, config, logger)
	traceID, err := generateTraceID(*event.WorkflowJob.RunID, *event.WorkflowJob.RunAttempt)

	if err != nil {
		logger.Error("Failed to generate trace ID", zap.Error(err))
		return traces, fmt.Errorf("failed to generate trace ID")
	}

	parentSpanID := createParentSpan(scopeSpans, event.WorkflowJob.Steps, event.WorkflowJob, traceID, logger)
	processSteps(scopeSpans, *event.WorkflowJob, traceID, parentSpanID, logger)

	return traces, nil
}

func processWorflowRunEvent(event *github.WorkflowRunEvent, config *Config, logger *zap.Logger) (ptrace.Traces, error) {
	traces := ptrace.NewTraces()
	resourceSpans := traces.ResourceSpans().AppendEmpty()

	logger.Info("Processing WorkflowRunEvent", zap.String("workflow_name", *event.WorkflowRun.Name), zap.String("repo", *event.Repo.FullName))
	runResource := resourceSpans.Resource()
	traceID, err := generateTraceID(*event.WorkflowRun.ID, int64(*event.WorkflowRun.RunAttempt))

	if err != nil {
		logger.Error("Failed to generate trace ID", zap.Error(err))
		return traces, fmt.Errorf("failed to generate trace ID")
	}

	createResourceAttributes(runResource, event, config, logger)
	createRootSpan(resourceSpans, event, traceID, logger)

	return traces, nil
}

func createParentSpan(scopeSpans ptrace.ScopeSpans, steps []*github.TaskStep, job *github.WorkflowJob, traceID pcommon.TraceID, logger *zap.Logger) pcommon.SpanID {
	logger.Debug("Creating parent span", zap.String("name", *job.Name))
	span := scopeSpans.Spans().AppendEmpty()
	span.SetTraceID(traceID)

	parentSpanID, _ := generateParentSpanID(*job.RunID, *job.RunAttempt)
	span.SetParentSpanID(parentSpanID)

	jobSpanID, _ := generateJobSpanID(*job.ID, *job.RunAttempt, *job.Name)
	span.SetSpanID(jobSpanID)

	span.SetName(*job.Name)
	span.SetKind(ptrace.SpanKindServer)
	if len(steps) > 0 {
		setSpanTimes(span, *steps[0].StartedAt, *steps[len(steps)-1].CompletedAt)
	} else {
		logger.Warn("No steps found, defaulting to job times")
		setSpanTimes(span, *job.CreatedAt, *job.CompletedAt)
	}

	allSuccessful := true
	anyFailure := false
	for _, step := range steps {
		if *step.Status != "completed" || *step.Conclusion != "success" {
			allSuccessful = false
		}
		if *step.Conclusion == "failure" {
			anyFailure = true
			break
		}
	}

	if anyFailure {
		span.Status().SetCode(ptrace.StatusCodeError)
	} else if allSuccessful {
		span.Status().SetCode(ptrace.StatusCodeOk)
	} else {
		span.Status().SetCode(ptrace.StatusCodeUnset)
	}

	span.Status().SetMessage(*job.Conclusion)

	return span.SpanID()
}

func createResourceAttributes(resource pcommon.Resource, event interface{}, config *Config, logger *zap.Logger) {
	attrs := resource.Attributes()

	switch e := event.(type) {
	case *github.WorkflowJobEvent:
		serviceName := generateServiceName(config, *e.Repo.FullName)
		attrs.PutStr("service.name", serviceName)

		attrs.PutStr("ci.github.workflow.name", *e.WorkflowJob.WorkflowName)

		attrs.PutStr("ci.github.workflow.job.created_at", e.WorkflowJob.CreatedAt.Format(time.RFC3339))
		attrs.PutStr("ci.github.workflow.job.completed_at", e.WorkflowJob.CompletedAt.Format(time.RFC3339))
		attrs.PutStr("ci.github.workflow.job.conclusion", *e.WorkflowJob.Conclusion)
		attrs.PutStr("ci.github.workflow.job.head_branch", *e.WorkflowJob.HeadBranch)
		attrs.PutStr("ci.github.workflow.job.head_sha", *e.WorkflowJob.HeadSHA)
		attrs.PutStr("ci.github.workflow.job.html_url", *e.WorkflowJob.HTMLURL)
		attrs.PutInt("ci.github.workflow.job.id", *e.WorkflowJob.ID)
		if len(e.WorkflowJob.Labels) > 0 {
			for i, label := range e.WorkflowJob.Labels {
				e.WorkflowJob.Labels[i] = strings.ToLower(label)
			}
			sort.Strings(e.WorkflowJob.Labels)
			joinedLabels := strings.Join(e.WorkflowJob.Labels, ",")
			attrs.PutStr("ci.github.workflow.job.labels", joinedLabels)
		} else {
			attrs.PutStr("ci.github.workflow.job.labels", "no labels")
		}

		attrs.PutStr("ci.github.workflow.job.labels", e.WorkflowJob.Labels[0])
		attrs.PutStr("ci.github.workflow.job.name", *e.WorkflowJob.Name)
		attrs.PutInt("ci.github.workflow.job.run_attempt", int64(*e.WorkflowJob.RunAttempt))
		attrs.PutInt("ci.github.workflow.job.run_id", *e.WorkflowJob.RunID)
		attrs.PutStr("ci.github.workflow.job.runner.group_name", *e.WorkflowJob.RunnerGroupName)
		attrs.PutStr("ci.github.workflow.job.runner.name", *e.WorkflowJob.RunnerName)
		attrs.PutStr("ci.github.workflow.job.sender.login", *e.Sender.Login)
		attrs.PutStr("ci.github.workflow.job.started_at", e.WorkflowJob.StartedAt.Format(time.RFC3339))
		attrs.PutStr("ci.github.workflow.job.status", *e.WorkflowJob.Status)

		attrs.PutStr("ci.system", "github")

		attrs.PutStr("scm.git.repo.owner.login", *e.Repo.Owner.Login)
		attrs.PutStr("scm.git.repo", *e.Repo.FullName)

	case *github.WorkflowRunEvent:
		serviceName := generateServiceName(config, *e.Repo.FullName)
		attrs.PutStr("service.name", serviceName)

		attrs.PutStr("ci.github.workflow.run.actor.login", *e.WorkflowRun.Actor.Login)

		attrs.PutStr("ci.github.workflow.run.conclusion", *e.WorkflowRun.Conclusion)
		attrs.PutStr("ci.github.workflow.run.created_at", (*e.WorkflowRun.CreatedAt).Format(time.RFC3339))
		attrs.PutStr("ci.github.workflow.run.display_title", *e.WorkflowRun.DisplayTitle)
		attrs.PutStr("ci.github.workflow.run.event", *e.WorkflowRun.Event)
		attrs.PutStr("ci.github.workflow.run.head_branch", *e.WorkflowRun.HeadBranch)
		attrs.PutStr("ci.github.workflow.run.head_sha", *e.WorkflowRun.HeadSHA)
		attrs.PutStr("ci.github.workflow.run.html_url", *e.WorkflowRun.HTMLURL)
		attrs.PutInt("ci.github.workflow.run.id", *e.WorkflowRun.ID)
		attrs.PutStr("ci.github.workflow.run.name", *e.WorkflowRun.Name)
		// attrs.PutStr("ci.github.workflow.run.path", *e.WorkflowRun.Path)
		if *e.WorkflowRun.PreviousAttemptURL != "" {
			htmlURL := transformGitHubAPIURL(*e.WorkflowRun.PreviousAttemptURL)
			attrs.PutStr("ci.github.workflow.run.previous_attempt_url", htmlURL)
		}
		attrs.PutInt("ci.github.workflow.run.run_attempt", int64(*e.WorkflowRun.RunAttempt))
		attrs.PutStr("ci.github.workflow.run.run_started_at", e.WorkflowRun.RunStartedAt.Format(time.RFC3339))
		attrs.PutStr("ci.github.workflow.run.status", *e.WorkflowRun.Status)
		attrs.PutStr("ci.github.workflow.run.updated_at", e.WorkflowRun.UpdatedAt.Format(time.RFC3339))

		attrs.PutStr("ci.github.workflow.run.sender.login", *e.Sender.Login)
		attrs.PutStr("ci.github.workflow.run.triggering_actor.login", *e.WorkflowRun.TriggeringActor.Login)
		attrs.PutStr("ci.github.workflow.run.updated_at", e.WorkflowRun.UpdatedAt.Format(time.RFC3339))

		attrs.PutStr("ci.system", "github")

		attrs.PutStr("scm.system", "git")

		attrs.PutStr("scm.git.head_branch", *e.WorkflowRun.HeadBranch)
		attrs.PutStr("scm.git.head_commit.author.email", *e.WorkflowRun.HeadCommit.Author.Email)
		attrs.PutStr("scm.git.head_commit.author.name", *e.WorkflowRun.HeadCommit.Author.Name)
		attrs.PutStr("scm.git.head_commit.committer.email", *e.WorkflowRun.HeadCommit.Committer.Email)
		attrs.PutStr("scm.git.head_commit.committer.name", *e.WorkflowRun.HeadCommit.Committer.Name)
		attrs.PutStr("scm.git.head_commit.message", *e.WorkflowRun.HeadCommit.Message)
		attrs.PutStr("scm.git.head_commit.timestamp", e.WorkflowRun.HeadCommit.Timestamp.Format(time.RFC3339))
		attrs.PutStr("scm.git.head_sha", *e.WorkflowRun.HeadSHA)

		if len(e.WorkflowRun.PullRequests) > 0 {
			var prUrls []string
			for _, pr := range e.WorkflowRun.PullRequests {
				prUrls = append(prUrls, convertPRURL(*pr.URL))
			}
			attrs.PutStr("scm.git.pull_requests.url", strings.Join(prUrls, ";"))
		}

		attrs.PutStr("scm.git.repo", *e.Repo.FullName)

	default:
		logger.Error("unknown event type", zap.String("event_type", fmt.Sprintf("%T", event)))
	}
}

func convertPRURL(apiURL string) string {
	apiURL = strings.Replace(apiURL, "/repos", "", 1)
	apiURL = strings.Replace(apiURL, "/pulls", "/pull", 1)
	return strings.Replace(apiURL, "api.", "", 1)
}

func createRootSpan(resourceSpans ptrace.ResourceSpans, event *github.WorkflowRunEvent, traceID pcommon.TraceID, logger *zap.Logger) (pcommon.SpanID, error) {
	logger.Debug("Creating root parent span", zap.String("name", *event.WorkflowRun.Name))
	scopeSpans := resourceSpans.ScopeSpans().AppendEmpty()
	span := scopeSpans.Spans().AppendEmpty()

	rootSpanID, err := generateParentSpanID(*event.WorkflowRun.ID, int64(*event.WorkflowRun.RunAttempt))
	if err != nil {
		logger.Error("Failed to generate root span ID", zap.Error(err))
		return pcommon.SpanID{}, err
	}

	span.SetTraceID(traceID)
	span.SetSpanID(rootSpanID)
	span.SetName(*event.WorkflowRun.Name)
	span.SetKind(ptrace.SpanKindServer)
	setSpanTimes(span, *event.WorkflowRun.RunStartedAt, *event.WorkflowRun.UpdatedAt)

	switch *event.WorkflowRun.Conclusion {
	case "success":
		span.Status().SetCode(ptrace.StatusCodeOk)
	case "failure":
		span.Status().SetCode(ptrace.StatusCodeError)
	default:
		span.Status().SetCode(ptrace.StatusCodeUnset)
	}

	span.Status().SetMessage(*event.WorkflowRun.Conclusion)

	// Attempt to link to previous trace ID if applicable
	if *event.WorkflowRun.PreviousAttemptURL != "" && *event.WorkflowRun.RunAttempt > 1 {
		logger.Debug("Linking to previous trace ID for WorkflowRunEvent")
		previousRunAttempt := *event.WorkflowRun.RunAttempt - 1
		previousTraceID, err := generateTraceID(*event.WorkflowRun.ID, int64(previousRunAttempt))
		if err != nil {
			logger.Error("Failed to generate previous trace ID", zap.Error(err))
		} else {
			link := span.Links().AppendEmpty()
			link.SetTraceID(previousTraceID)
			logger.Debug("Successfully linked to previous trace ID", zap.String("previousTraceID", previousTraceID.String()))
		}
	}

	return rootSpanID, nil
}

func createSpan(scopeSpans ptrace.ScopeSpans, step github.TaskStep, job github.WorkflowJob, traceID pcommon.TraceID, parentSpanID pcommon.SpanID, logger *zap.Logger) pcommon.SpanID {
	logger.Debug("Processing span", zap.String("step_name", *step.Name))
	span := scopeSpans.Spans().AppendEmpty()
	span.SetTraceID(traceID)
	span.SetParentSpanID(parentSpanID)

	var spanID pcommon.SpanID

	span.Attributes().PutStr("ci.github.workflow.job.step.name", *step.Name)
	span.Attributes().PutStr("ci.github.workflow.job.step.status", *step.Status)
	span.Attributes().PutStr("ci.github.workflow.job.step.conclusion", *step.Conclusion)

	spanID, _ = generateStepSpanID(*job.RunID, *job.RunAttempt, *job.Name, int(*step.Number))
	span.Attributes().PutInt("ci.github.workflow.job.step.number", int64(*step.Number))

	span.Attributes().PutStr("ci.github.workflow.job.step.started_at", step.StartedAt.Format(time.RFC3339))
	span.Attributes().PutStr("ci.github.workflow.job.step.completed_at", step.CompletedAt.Format(time.RFC3339))

	span.SetSpanID(spanID)

	setSpanTimes(span, *step.StartedAt, *step.CompletedAt)
	span.SetName(*step.Name)
	span.SetKind(ptrace.SpanKindServer)

	switch *step.Conclusion {
	case "success":
		span.Status().SetCode(ptrace.StatusCodeOk)
	case "failure":
		span.Status().SetCode(ptrace.StatusCodeError)
	default:
		span.Status().SetCode(ptrace.StatusCodeUnset)
	}

	span.Status().SetMessage(*step.Conclusion)

	return span.SpanID()
}

func generateTraceID(runID, runAttempt int64) (pcommon.TraceID, error) {
	input := fmt.Sprintf("%d%dt", runID, runAttempt)
	hash := sha256.Sum256([]byte(input))
	traceIDHex := hex.EncodeToString(hash[:])

	var traceID pcommon.TraceID
	_, err := hex.Decode(traceID[:], []byte(traceIDHex[:32]))
	if err != nil {
		return pcommon.TraceID{}, err
	}

	return traceID, nil
}

func generateJobSpanID(runID, runAttempt int64, job string) (pcommon.SpanID, error) {
	input := fmt.Sprintf("%d%d%s", runID, runAttempt, job)
	hash := sha256.Sum256([]byte(input))
	spanIDHex := hex.EncodeToString(hash[:])

	var spanID pcommon.SpanID
	_, err := hex.Decode(spanID[:], []byte(spanIDHex[16:32]))
	if err != nil {
		return pcommon.SpanID{}, err
	}

	return spanID, nil
}

func generateParentSpanID(runID, runAttempt int64) (pcommon.SpanID, error) {
	input := fmt.Sprintf("%d%ds", runID, runAttempt)
	hash := sha256.Sum256([]byte(input))
	spanIDHex := hex.EncodeToString(hash[:])

	var spanID pcommon.SpanID
	_, err := hex.Decode(spanID[:], []byte(spanIDHex[16:32]))
	if err != nil {
		return pcommon.SpanID{}, err
	}

	return spanID, nil
}

func generateServiceName(config *Config, fullName string) string {
	if config.CustomServiceName != "" {
		return config.CustomServiceName
	}
	formattedName := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(fullName, "/", "-"), "_", "-"))
	return fmt.Sprintf("%s%s%s", config.ServiceNamePrefix, formattedName, config.ServiceNameSuffix)
}

func generateStepSpanID(runID, runAttempt int64, jobName string, stepNumber int) (pcommon.SpanID, error) {
	var input string
	input = fmt.Sprintf("%d%d%s%s", runID, runAttempt, jobName, stepNumber)
	hash := sha256.Sum256([]byte(input))
	spanIDHex := hex.EncodeToString(hash[:])

	var spanID pcommon.SpanID
	_, err := hex.Decode(spanID[:], []byte(spanIDHex[16:32]))
	if err != nil {
		return pcommon.SpanID{}, err
	}

	return spanID, nil
}

func processSteps(scopeSpans ptrace.ScopeSpans, job github.WorkflowJob, traceID pcommon.TraceID, parentSpanID pcommon.SpanID, logger *zap.Logger) {
	for _, step := range job.Steps {
		createSpan(scopeSpans, *step, job, traceID, parentSpanID, logger)
	}
}

func setSpanTimes(span ptrace.Span, start, end github.Timestamp) {
	span.SetStartTimestamp(pcommon.NewTimestampFromTime(start.Time))
	span.SetEndTimestamp(pcommon.NewTimestampFromTime(end.Time))
}

func transformGitHubAPIURL(apiURL string) string {
	htmlURL := strings.Replace(apiURL, "api.github.com/repos", "github.com", 1)
	return htmlURL
}

func validateSignatureSHA256(secret string, signatureHeader string, body []byte, logger *zap.Logger) bool {
	if signatureHeader == "" || len(signatureHeader) < 7 {
		logger.Debug("Unauthorized - No Signature Header")
		return false
	}
	receivedSig := signatureHeader[7:]
	computedHash := hmac.New(sha256.New, []byte(secret))
	computedHash.Write(body)
	expectedSig := hex.EncodeToString(computedHash.Sum(nil))

	logger.Debug("Debugging Signatures", zap.String("Received", receivedSig), zap.String("Computed", expectedSig))

	return hmac.Equal([]byte(expectedSig), []byte(receivedSig))
}

func validateSignatureSHA1(secret string, signatureHeader string, body []byte, logger *zap.Logger) bool {
	if signatureHeader == "" {
		logger.Debug("Unauthorized - No Signature Header")
		return false
	}
	receivedSig := signatureHeader[5:] // Assume "sha1=" prefix
	computedHash := hmac.New(sha1.New, []byte(secret))
	computedHash.Write(body)
	expectedSig := hex.EncodeToString(computedHash.Sum(nil))

	logger.Debug("Debugging Signatures", zap.String("Received", receivedSig), zap.String("Computed", expectedSig))

	return hmac.Equal([]byte(expectedSig), []byte(receivedSig))
}

func (r *githubActionsReceiver) registerTracesConsumer(tc consumer.Traces) error {
	r.tracesConsumer = tc
	if tc == nil {
		return component.ErrNilNextConsumer
	}

	return nil
}

func (r *githubActionsReceiver) registerLogsConsumer(lc consumer.Logs) error {
	r.logsConsumer = lc
	if lc == nil {
		return component.ErrNilNextConsumer
	}

	return nil
}

func newReceiver(
	params receiver.CreateSettings,
	config *Config,
) *githubActionsReceiver {
	transport := "http"
	if config.TLSSetting != nil {
		transport = "https"
	}

	obsrecv, _ := receiverhelper.NewObsReport(receiverhelper.ObsReportSettings{
		ReceiverID:             params.ID,
		Transport:              transport,
		ReceiverCreateSettings: params,
	})

	client := github.NewClient(nil).WithAuthToken(config.Token)

	gar := &githubActionsReceiver{
		config:         config,
		createSettings: params,
		logger:         params.Logger,
		obsrecv:        obsrecv,
		ghClient:       client,
	}

	return gar
}

func (gar *githubActionsReceiver) Start(ctx context.Context, host component.Host) error {
	endpoint := fmt.Sprintf("%s%s", gar.config.Endpoint, gar.config.Path)
	gar.logger.Info("Starting GithubActions server", zap.String("endpoint", endpoint))
	gar.server = &http.Server{
		Addr:    gar.config.HTTPServerSettings.Endpoint,
		Handler: gar,
	}

	gar.shutdownWG.Add(1)
	go func() {
		defer gar.shutdownWG.Done()
		if err := gar.server.ListenAndServe(); err != http.ErrServerClosed {
			gar.createSettings.TelemetrySettings.ReportStatus(component.NewFatalErrorEvent(err))
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

func (gar *githubActionsReceiver) processWebhookPayload(blob []byte) (interface{}, error) {
	var event map[string]json.RawMessage
	if err := json.Unmarshal(blob, &event); err != nil {
		gar.logger.Error("Failed to unmarshal blob", zap.Error(err))
		return nil, err
	}

	// var traces ptrace.Traces
	if _, ok := event["workflow_job"]; ok {
		gar.logger.Debug("Unmarshalling WorkflowJobEvent")
		var jobEvent github.WorkflowJobEvent
		err := json.Unmarshal(blob, &jobEvent)

		if err != nil {
			gar.logger.Error("Failed to unmarshal job event", zap.Error(err))
			return nil, err
		}
		if *jobEvent.WorkflowJob.Status != "completed" {
			return nil, nil
		}

		return jobEvent, nil
	} else if _, ok := event["workflow_run"]; ok {
		gar.logger.Debug("Unmarshalling WorkflowRunEvent")
		var runEvent github.WorkflowRunEvent
		err := json.Unmarshal(blob, &runEvent)

		if err != nil {
			gar.logger.Error("Failed to unmarshal run event", zap.Error(err))
			return nil, err
		}
		if *runEvent.WorkflowRun.Status != "completed" {
			return nil, nil
		}

		return runEvent, nil
	}

	gar.logger.Warn("Unknown event type")
	return nil, fmt.Errorf("unknown event type: %v", event)
}

func (gar *githubActionsReceiver) processLogs(e github.WorkflowRunEvent) {
	serviceName := generateServiceName(gar.config, *e.Repo.FullName)
	traceID, _ := generateTraceID(*e.WorkflowRun.ID, int64(*e.WorkflowRun.RunAttempt))

	logs := plog.NewLogs()
	allLogs := logs.ResourceLogs().AppendEmpty()
	allAttrs := allLogs.Resource().Attributes()

	allAttrs.PutStr("traceId", traceID.String())
	allAttrs.PutStr("service.name", serviceName)

	allAttrs.PutStr("ci.github.workflow.run.actor.login", *e.WorkflowRun.Actor.Login)

	allAttrs.PutStr("ci.github.workflow.run.conclusion", *e.WorkflowRun.Conclusion)
	allAttrs.PutStr("ci.github.workflow.run.created_at", (*e.WorkflowRun.CreatedAt).Format(time.RFC3339))
	allAttrs.PutStr("ci.github.workflow.run.display_title", *e.WorkflowRun.DisplayTitle)
	allAttrs.PutStr("ci.github.workflow.run.event", *e.WorkflowRun.Event)
	allAttrs.PutStr("ci.github.workflow.run.head_branch", *e.WorkflowRun.HeadBranch)
	allAttrs.PutStr("ci.github.workflow.run.head_sha", *e.WorkflowRun.HeadSHA)
	allAttrs.PutStr("ci.github.workflow.run.html_url", *e.WorkflowRun.HTMLURL)
	allAttrs.PutInt("ci.github.workflow.run.id", *e.WorkflowRun.ID)
	allAttrs.PutStr("ci.github.workflow.run.name", *e.WorkflowRun.Name)
	// attrs.PutStr("ci.github.workflow.run.path", *e.WorkflowRun.Path)
	if *e.WorkflowRun.PreviousAttemptURL != "" {
		htmlURL := transformGitHubAPIURL(*e.WorkflowRun.PreviousAttemptURL)
		allAttrs.PutStr("ci.github.workflow.run.previous_attempt_url", htmlURL)
	}
	allAttrs.PutInt("ci.github.workflow.run.run_attempt", int64(*e.WorkflowRun.RunAttempt))
	allAttrs.PutStr("ci.github.workflow.run.run_started_at", e.WorkflowRun.RunStartedAt.Format(time.RFC3339))
	allAttrs.PutStr("ci.github.workflow.run.status", *e.WorkflowRun.Status)
	allAttrs.PutStr("ci.github.workflow.run.updated_at", e.WorkflowRun.UpdatedAt.Format(time.RFC3339))

	allAttrs.PutStr("ci.github.workflow.run.sender.login", *e.Sender.Login)
	allAttrs.PutStr("ci.github.workflow.run.triggering_actor.login", *e.WorkflowRun.TriggeringActor.Login)
	allAttrs.PutStr("ci.github.workflow.run.updated_at", e.WorkflowRun.UpdatedAt.Format(time.RFC3339))

	allAttrs.PutStr("ci.system", "github")

	allAttrs.PutStr("scm.system", "git")

	allAttrs.PutStr("scm.git.head_branch", *e.WorkflowRun.HeadBranch)
	allAttrs.PutStr("scm.git.head_commit.author.email", *e.WorkflowRun.HeadCommit.Author.Email)
	allAttrs.PutStr("scm.git.head_commit.author.name", *e.WorkflowRun.HeadCommit.Author.Name)
	allAttrs.PutStr("scm.git.head_commit.committer.email", *e.WorkflowRun.HeadCommit.Committer.Email)
	allAttrs.PutStr("scm.git.head_commit.committer.name", *e.WorkflowRun.HeadCommit.Committer.Name)
	allAttrs.PutStr("scm.git.head_commit.message", *e.WorkflowRun.HeadCommit.Message)
	allAttrs.PutStr("scm.git.head_commit.timestamp", e.WorkflowRun.HeadCommit.Timestamp.Format(time.RFC3339))
	allAttrs.PutStr("scm.git.head_sha", *e.WorkflowRun.HeadSHA)

	if len(e.WorkflowRun.PullRequests) > 0 {
		var prUrls []string
		for _, pr := range e.WorkflowRun.PullRequests {
			prUrls = append(prUrls, convertPRURL(*pr.URL))
		}
		allAttrs.PutStr("scm.git.pull_requests.url", strings.Join(prUrls, ";"))
	}
	allAttrs.PutStr("scm.git.repo", *e.Repo.FullName)

	url, _, err := gar.ghClient.Actions.GetWorkflowRunAttemptLogs(context.Background(), *e.Repo.Owner.Login, *e.Repo.Name, *e.WorkflowRun.ID, int(*e.WorkflowRun.RunAttempt), 10)

	if err != nil {
		gar.logger.Error("Failed to get logs", zap.Error(err))
		return
	}

	out, err := os.CreateTemp("", "tmpfile-")
	if err != nil {
		gar.logger.Error("Failed to create temp file", zap.Error(err))
		return
	}
	defer out.Close()
	defer os.Remove(out.Name())

	resp, err := http.Get(url.String())
	if err != nil {
		gar.logger.Error("Failed to get logs", zap.Error(err))
		return
	}
	defer resp.Body.Close()

	// Copy the response into the temp file
	io.Copy(out, resp.Body)

	archive, _ := zip.OpenReader(out.Name())
	defer archive.Close()

	// steps is a map of job names to a map of step numbers to file names
	var jobs = make([]string, 0)
	var files = make([]*zip.File, 0)

	// first we get all the directories. each directory is a job
	for _, f := range archive.File {
		if f.FileInfo().IsDir() {
			// if the file is a directory, then it's a job. each file in this directory is a step
			jobs = append(jobs, f.Name[:len(f.Name)-1])
		} else {
			files = append(files, f)
		}
	}

	for _, jobName := range jobs {

		jobLogsScope := allLogs.ScopeLogs().AppendEmpty()
		jobLogsScope.Scope().Attributes().PutStr("ci.github.workflow.job.name", jobName)

		for _, logFile := range files {

			if strings.HasPrefix(logFile.Name, jobName) {
				fileNameWithoutDir := strings.Replace(logFile.Name, jobName+"/", "", 1)
				stepNumber, _ := strconv.Atoi(strings.Split(fileNameWithoutDir, "_")[0])

				spanId, _ := generateStepSpanID(*e.WorkflowRun.ID, int64(*e.WorkflowRun.RunAttempt), jobName, stepNumber)

				ff, err := logFile.Open()
				if err != nil {
					gar.logger.Error("Failed to open file", zap.Error(err))
					break
				}

				scanner := bufio.NewScanner(ff)
				for scanner.Scan() {
					record := jobLogsScope.LogRecords().AppendEmpty()

					record.SetSpanID(spanId)
					record.SetTraceID(traceID)

					record.Attributes().PutInt("ci.github.workflow.job.step.number", int64(stepNumber))

					now := pcommon.NewTimestampFromTime(time.Now())

					ts, line, _ := strings.Cut(scanner.Text(), " ")
					parsedTime, _ := time.Parse(time.RFC3339, ts)

					record.SetObservedTimestamp(now)
					record.SetTimestamp(pcommon.NewTimestampFromTime(parsedTime))

					// TODO(maybe): handle log groups and sections? filter them out?
					record.Body().SetStr(line)
				}
				ff.Close()

				if err := scanner.Err(); err != nil {
					gar.logger.Error("error reading file", zap.Error(err))
				}

			}
		}

	}

	consumerErr := gar.logsConsumer.ConsumeLogs(context.Background(), logs)
	if consumerErr != nil {
		gar.logger.Error("Failed to process logs", zap.Error(consumerErr))
		return
	}
}

func (gar *githubActionsReceiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userAgent := r.Header.Get("User-Agent")
	if !strings.HasPrefix(userAgent, "GitHub-Hookshot") {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	if r.URL.Path != gar.config.Path {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	defer r.Body.Close()

	slurp, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusInternalServerError)
		return
	}

	// Validate the request if Secret is set in the configuration
	if gar.config.Secret != "" {
		signatureSHA256 := r.Header.Get("X-Hub-Signature-256")
		if signatureSHA256 != "" && !validateSignatureSHA256(gar.config.Secret, signatureSHA256, slurp, gar.logger) {
			gar.logger.Debug("Unauthorized - Signature Mismatch SHA256")
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		} else {
			signatureSHA1 := r.Header.Get("X-Hub-Signature")
			if signatureSHA1 != "" && !validateSignatureSHA1(gar.config.Secret, signatureSHA1, slurp, gar.logger) {
				gar.logger.Debug("Unauthorized - Signature Mismatch SHA1")
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
		}
	}

	gar.logger.Debug("Received request", zap.ByteString("payload", slurp))

	event, err := gar.processWebhookPayload(slurp)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// If event is nil, it means the event was not a "completed" event
	if event == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	var traces ptrace.Traces

	switch e := event.(type) {
	case github.WorkflowJobEvent:
		traces, err = processWorflowJobEvent(&e, gar.config, gar.logger)
		if err != nil {
			gar.logger.Error("Failed to convert event to traces", zap.Error(err))
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

	case github.WorkflowRunEvent:
		traces, err = processWorflowRunEvent(&e, gar.config, gar.logger)
		if err != nil {
			gar.logger.Error("Failed to convert event to traces", zap.Error(err))
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// there's a log pipeline for this receiver, request logs
		if gar.logsConsumer != nil {
			go gar.processLogs(e)
		}

	}

	gar.logger.Debug("Unmarshaled spans", zap.Int("#spans", traces.SpanCount()))

	// There's a trace pipeline for this receiver
	if gar.tracesConsumer != nil {

		// Pass the traces to the nextConsumer
		consumerErr := gar.tracesConsumer.ConsumeTraces(ctx, traces)
		if consumerErr != nil {
			gar.logger.Error("Failed to process traces", zap.Error(consumerErr))
			http.Error(w, "Failed to process traces", http.StatusInternalServerError)
			return
		}
	}

	w.WriteHeader(http.StatusAccepted)
}
