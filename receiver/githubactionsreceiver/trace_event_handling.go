// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package githubactionsreceiver

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/google/go-github/v60/github"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.uber.org/zap"
)

func eventToTraces(event interface{}, config *Config, logger *zap.Logger) (*ptrace.Traces, error) {
	logger.Debug("Determining event")
	traces := ptrace.NewTraces()
	resourceSpans := traces.ResourceSpans().AppendEmpty()
	scopeSpans := resourceSpans.ScopeSpans().AppendEmpty()

	switch e := event.(type) {
	case *github.WorkflowJobEvent:
		if e.GetWorkflowJob().GetStatus() != "completed" {
			logger.Debug("Job not completed, skipping")
			return nil, nil
		}

		logger.Info("Processing WorkflowJobEvent", zap.String("job_name", e.GetWorkflowJob().GetName()), zap.String("repo", e.GetRepo().GetFullName()))
		jobResource := resourceSpans.Resource()
		createResourceAttributes(jobResource, e, config, logger)

		traceID, err := generateTraceID(e.GetWorkflowJob().GetRunID(), int(e.GetWorkflowJob().GetRunAttempt()))
		if err != nil {
			logger.Error("Failed to generate trace ID", zap.Error(err))
			return nil, fmt.Errorf("failed to generate trace ID: %w", err)
		}

		parentSpanID := createParentSpan(scopeSpans, e.GetWorkflowJob().Steps, e.GetWorkflowJob(), traceID, logger)
		processSteps(scopeSpans, e.GetWorkflowJob().Steps, e.GetWorkflowJob(), traceID, parentSpanID, logger)

	case *github.WorkflowRunEvent:
		if e.GetWorkflowRun().GetStatus() != "completed" {
			logger.Debug("Run not completed, skipping")
			return nil, nil
		}
		logger.Info("Processing WorkflowRunEvent", zap.String("workflow_name", e.GetWorkflowRun().GetName()), zap.String("repo", e.GetRepo().GetFullName()))
		runResource := resourceSpans.Resource()

		traceID, err := generateTraceID(e.GetWorkflowRun().GetID(), e.GetWorkflowRun().GetRunAttempt())
		if err != nil {
			logger.Error("Failed to generate trace ID", zap.Error(err))
			return nil, fmt.Errorf("failed to generate trace ID: %w", err)
		}

		createResourceAttributes(runResource, e, config, logger)
		createRootSpan(resourceSpans, e, traceID, logger)

	default:
		logger.Error("unknown event type, dropping payload")
		return nil, fmt.Errorf("unknown event type")
	}

	return &traces, nil
}

func createParentSpan(scopeSpans ptrace.ScopeSpans, steps []*github.TaskStep, job *github.WorkflowJob, traceID pcommon.TraceID, logger *zap.Logger) pcommon.SpanID {
	logger.Debug("Creating parent span", zap.String("name", job.GetName()))
	span := scopeSpans.Spans().AppendEmpty()
	span.SetTraceID(traceID)

	parentSpanID, _ := generateParentSpanID(job.GetRunID(), int(job.GetRunAttempt()))
	span.SetParentSpanID(parentSpanID)

	jobSpanID, _ := generateJobSpanID(job.GetRunID(), int(job.GetRunAttempt()), job.GetName())
	span.SetSpanID(jobSpanID)

	span.SetName(job.GetName())
	span.SetKind(ptrace.SpanKindServer)
	if len(steps) > 0 {
		setSpanTimes(span, steps[0].GetStartedAt().Time, steps[len(steps)-1].GetCompletedAt().Time)
	} else {
		// Invoked when status skipped or cancelled
		logger.Warn("No steps found, defaulting to job times")
		setSpanTimes(span, job.GetStartedAt().Time, job.GetCompletedAt().Time)
	}

	allSuccessful := true
	anyFailure := false
	for _, step := range steps {
		if step.GetStatus() != "completed" || step.GetConclusion() != "success" {
			allSuccessful = false
		}
		if step.GetConclusion() == "failure" {
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

	span.Status().SetMessage(job.GetConclusion())

	return span.SpanID()
}

func convertPRURL(apiURL string) string {
	apiURL = strings.Replace(apiURL, "/repos", "", 1)
	apiURL = strings.Replace(apiURL, "/pulls", "/pull", 1)
	return strings.Replace(apiURL, "api.", "", 1)
}

func createRootSpan(resourceSpans ptrace.ResourceSpans, event *github.WorkflowRunEvent, traceID pcommon.TraceID, logger *zap.Logger) (pcommon.SpanID, error) {
	logger.Debug("Creating root parent span", zap.String("name", event.GetWorkflowRun().GetName()))
	scopeSpans := resourceSpans.ScopeSpans().AppendEmpty()
	span := scopeSpans.Spans().AppendEmpty()

	rootSpanID, err := generateParentSpanID(event.GetWorkflowRun().GetID(), event.GetWorkflowRun().GetRunAttempt())
	if err != nil {
		logger.Error("Failed to generate root span ID", zap.Error(err))
		return pcommon.SpanID{}, fmt.Errorf("failed to generate root span ID: %w", err)
	}

	span.SetTraceID(traceID)
	span.SetSpanID(rootSpanID)
	span.SetName(event.GetWorkflowRun().GetName())
	span.SetKind(ptrace.SpanKindServer)
	setSpanTimes(span, event.GetWorkflowRun().GetRunStartedAt().Time, event.GetWorkflowRun().GetUpdatedAt().Time)

	switch event.WorkflowRun.GetConclusion() {
	case "success":
		span.Status().SetCode(ptrace.StatusCodeOk)
	case "failure":
		span.Status().SetCode(ptrace.StatusCodeError)
	default:
		span.Status().SetCode(ptrace.StatusCodeUnset)
	}

	span.Status().SetMessage(event.GetWorkflowRun().GetConclusion())

	// Attempt to link to previous trace ID if applicable
	if event.GetWorkflowRun().GetPreviousAttemptURL() != "" && event.GetWorkflowRun().GetRunAttempt() > 1 {
		logger.Debug("Linking to previous trace ID for WorkflowRunEvent")
		previousRunAttempt := event.GetWorkflowRun().GetRunAttempt() - 1
		previousTraceID, err := generateTraceID(event.GetWorkflowRun().GetID(), previousRunAttempt)
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

func createSpan(scopeSpans ptrace.ScopeSpans, step *github.TaskStep, job *github.WorkflowJob, traceID pcommon.TraceID, parentSpanID pcommon.SpanID, logger *zap.Logger, stepNumber int) pcommon.SpanID {
	logger.Debug("Processing span", zap.String("step_name", step.GetName()))
	span := scopeSpans.Spans().AppendEmpty()
	span.SetTraceID(traceID)
	span.SetParentSpanID(parentSpanID)

	var spanID pcommon.SpanID

	span.Attributes().PutStr("ci.github.workflow.job.step.name", step.GetName())
	span.Attributes().PutStr("ci.github.workflow.job.step.status", step.GetStatus())
	span.Attributes().PutStr("ci.github.workflow.job.step.conclusion", step.GetConclusion())
	spanID, _ = generateStepSpanID(job.GetRunID(), int(job.GetRunAttempt()), job.GetName(), stepNumber)
	span.Attributes().PutInt("ci.github.workflow.job.step.number", int64(stepNumber))
	span.Attributes().PutStr("ci.github.workflow.job.step.started_at", step.GetStartedAt().Format(time.RFC3339))
	span.Attributes().PutStr("ci.github.workflow.job.step.completed_at", step.GetCompletedAt().Format(time.RFC3339))

	span.SetSpanID(spanID)

	setSpanTimes(span, step.GetStartedAt().Time, step.GetCompletedAt().Time)
	span.SetName(step.GetName())
	span.SetKind(ptrace.SpanKindServer)

	switch step.GetConclusion() {
	case "success":
		span.Status().SetCode(ptrace.StatusCodeOk)
	case "failure":
		span.Status().SetCode(ptrace.StatusCodeError)
	default:
		span.Status().SetCode(ptrace.StatusCodeUnset)
	}

	span.Status().SetMessage(step.GetConclusion())

	return span.SpanID()
}

func generateTraceID(runID int64, runAttempt int) (pcommon.TraceID, error) {
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

func generateJobSpanID(runID int64, runAttempt int, job string) (pcommon.SpanID, error) {
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

func generateParentSpanID(runID int64, runAttempt int) (pcommon.SpanID, error) {
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

func generateStepSpanID(runID int64, runAttempt int, jobName string, stepNumber int) (pcommon.SpanID, error) {
	var input string
	input = fmt.Sprintf("%d%d%s%d", runID, runAttempt, jobName, stepNumber)
	hash := sha256.Sum256([]byte(input))
	spanIDHex := hex.EncodeToString(hash[:])

	var spanID pcommon.SpanID
	_, err := hex.Decode(spanID[:], []byte(spanIDHex[16:32]))
	if err != nil {
		return pcommon.SpanID{}, err
	}

	return spanID, nil
}

func processSteps(scopeSpans ptrace.ScopeSpans, steps []*github.TaskStep, job *github.WorkflowJob, traceID pcommon.TraceID, parentSpanID pcommon.SpanID, logger *zap.Logger) {
	for _, step := range steps {
		createSpan(scopeSpans, step, job, traceID, parentSpanID, logger, int(step.GetNumber())) // Pass step number if duplicate names exist
	}
}

func setSpanTimes(span ptrace.Span, start, end time.Time) {
	span.SetStartTimestamp(pcommon.NewTimestampFromTime(start))
	span.SetEndTimestamp(pcommon.NewTimestampFromTime(end))
}

func transformGitHubAPIURL(apiURL string) string {
	htmlURL := strings.Replace(apiURL, "api.github.com/repos", "github.com", 1)
	return htmlURL
}
