// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package githubactionsreceiver // import "github.com/open-telemetry/opentelemetry-collector-contrib/receiver/githubactionsreceiver"

import (
	"context"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/confighttp"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/receiver"

	"github.com/open-telemetry/opentelemetry-collector-contrib/internal/sharedcomponent"
	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/githubactionsreceiver/internal/metadata"
)

// This file implements factory for GitHub Actions receiver.

const (
	defaultBindEndpoint = "0.0.0.0:19418"
	defaultPath         = "/ghaevents"
)

// NewFactory creates a new GitHub Actions receiver factory
func NewFactory() receiver.Factory {
	return receiver.NewFactory(
		metadata.Type,
		createDefaultConfig,
		receiver.WithTraces(createTracesReceiver, metadata.TracesStability),
	)
}

// createDefaultConfig creates the default configuration for GitHub Actions receiver.
func createDefaultConfig() component.Config {
	return &Config{
		HTTPServerSettings: confighttp.HTTPServerSettings{
			Endpoint: defaultBindEndpoint,
		},
		Path:   defaultPath,
		Secret: "",
	}
}

// createTracesReceiver creates a trace receiver based on provided config.
func createTracesReceiver(
	_ context.Context,
	set receiver.CreateSettings,
	cfg component.Config,
	nextConsumer consumer.Traces,
) (receiver.Traces, error) {
	rCfg := cfg.(*Config)

	r := receivers.GetOrAdd(cfg, func() component.Component {
		return newReceiver(set, rCfg)
	})

	if err := r.Unwrap().(*githubActionsReceiver).registerTracesConsumer(nextConsumer); err != nil {
		return nil, err
	}

	return r, nil
}

// createTracesReceiver creates a trace receiver based on provided config.
func createLogsReceiver(
	_ context.Context,
	set receiver.CreateSettings,
	cfg component.Config,
	nextConsumer consumer.Logs,
) (receiver.Traces, error) {
	rCfg := cfg.(*Config)

	r := receivers.GetOrAdd(cfg, func() component.Component {
		return newReceiver(set, rCfg)
	})

	if err := r.Unwrap().(*githubActionsReceiver).registerLogsConsumer(nextConsumer); err != nil {
		return nil, err
	}

	return r, nil
}

// the receiver is able to handle all types of data, we only create one instance per ID
var receivers = sharedcomponent.NewSharedComponents()
