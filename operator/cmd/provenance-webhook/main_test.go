// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func TestManagerStartsProvenanceWebhook(t *testing.T) {
	// Reuse a loopback test certificate and port without cluster credentials.
	tlsServer := httptest.NewTLSServer(http.NotFoundHandler())
	endpoint, err := url.Parse(tlsServer.URL)
	require.NoError(t, err)
	port, err := strconv.Atoi(endpoint.Port())
	require.NoError(t, err)
	certificate := tlsServer.TLS.Certificates[0]
	client := tlsServer.Client()
	tlsServer.Close()

	server := webhook.NewServer(webhook.Options{
		Host: endpoint.Hostname(), Port: port,
		TLSOpts: []func(*tls.Config){func(config *tls.Config) {
			config.GetCertificate = func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
				return &certificate, nil
			}
		}},
	})
	mgr, err := ctrl.NewManager(&rest.Config{Host: "http://127.0.0.1:1"}, ctrl.Options{
		WebhookServer: server, HealthProbeBindAddress: "0",
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	require.NoError(t, err)
	require.NoError(t, setupWebhook(mgr, admission.HandlerFunc(func(context.Context, admission.Request) admission.Response {
		return admission.Allowed("test request")
	})))
	checker := server.StartedChecker()
	require.Error(t, checker(nil), "readiness must fail before manager startup")

	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() { finished <- mgr.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-finished:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Error("manager did not stop after cancellation")
		}
	})
	require.Eventually(t, func() bool { return checker(nil) == nil }, 5*time.Second, 10*time.Millisecond)
	client.Timeout = time.Second
	response, err := client.Get(tlsServer.URL + "/validate-shadowpod")
	require.NoError(t, err)
	defer response.Body.Close()
	// An empty request must reach admission validation, rather than a missing route.
	require.Equal(t, http.StatusOK, response.StatusCode)
	var review admissionv1.AdmissionReview
	require.NoError(t, json.NewDecoder(response.Body).Decode(&review))
	require.NotNil(t, review.Response)
	require.False(t, review.Response.Allowed)
	require.NotNil(t, review.Response.Result)
	require.EqualValues(t, http.StatusBadRequest, review.Response.Result.Code)
}
