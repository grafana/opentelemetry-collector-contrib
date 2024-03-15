// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package githubactionsreceiver

import (
	"archive/zip"
	"bufio"
	"context"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-github/v61/github"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.uber.org/zap"
)

func eventToLogs(event interface{}, config *Config, ghClient *github.Client, logger *zap.Logger, withTraceInfo bool) (*plog.Logs, error) {
	if e, ok := event.(*github.WorkflowRunEvent); ok {
		if e.GetWorkflowRun().GetStatus() != "completed" {
			logger.Debug("Run not completed, skipping")
			return nil, nil
		}

		traceID, _ := generateTraceID(e.GetWorkflowRun().GetID(), e.GetWorkflowRun().GetRunAttempt())

		logs := plog.NewLogs()
		allLogs := logs.ResourceLogs().AppendEmpty()
		attrs := allLogs.Resource().Attributes()

		setWorkflowRunEventAttributes(attrs, e, config)

		url, _, err := ghClient.Actions.GetWorkflowRunAttemptLogs(context.Background(), e.GetRepo().GetOwner().GetLogin(), e.GetRepo().GetName(), e.GetWorkflowRun().GetID(), e.GetWorkflowRun().GetRunAttempt(), 10)

		if err != nil {
			logger.Error("Failed to get logs", zap.Error(err))
			return nil, err
		}

		out, err := os.CreateTemp("", "tmpfile-")
		if err != nil {
			logger.Error("Failed to create temp file", zap.Error(err))
			return nil, err
		}
		defer out.Close()
		defer os.Remove(out.Name())

		resp, err := http.Get(url.String())
		if err != nil {
			logger.Error("Failed to get logs", zap.Error(err))
			return nil, err
		}
		defer resp.Body.Close()

		// Copy the response into the temp file
		_, err = io.Copy(out, resp.Body)
		if err != nil {
			logger.Error("Failed to copy response to temp file", zap.Error(err))
			return nil, err
		}

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

					spanID, _ := generateStepSpanID(e.GetWorkflowRun().GetID(), e.GetWorkflowRun().GetRunAttempt(), jobName, int64(stepNumber))

					ff, err := logFile.Open()
					if err != nil {
						logger.Error("Failed to open file", zap.Error(err))
						break
					}

					scanner := bufio.NewScanner(ff)
					for scanner.Scan() {
						record := jobLogsScope.LogRecords().AppendEmpty()

						if withTraceInfo {
							record.SetSpanID(spanID)
							record.SetTraceID(traceID)
						}

						record.Attributes().PutInt("ci.github.workflow.job.step.number", int64(stepNumber))

						now := pcommon.NewTimestampFromTime(time.Now())

						ts, line, _ := strings.Cut(scanner.Text(), " ")
						parsedTime, _ := time.Parse(time.RFC3339, ts)

						record.SetObservedTimestamp(now)
						record.SetTimestamp(pcommon.NewTimestampFromTime(parsedTime))

						record.Body().SetStr(line)
					}
					ff.Close()

					if err := scanner.Err(); err != nil {
						logger.Error("error reading file", zap.Error(err))
					}

				}
			}

		}

		return &logs, nil
	}

	return nil, nil
}
