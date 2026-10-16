// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	"sigs.k8s.io/yaml"

	remote "github.com/ai-dynamo/snapshot/agent/pkg/provenance"
	"github.com/ai-dynamo/snapshot/operator/internal/provenance"
)

func main() {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	bindingPath := flag.String("binding-config", "/etc/snapshot-provenance/binding.yaml", "Provider binding configuration")
	certDir := flag.String("tls-cert-dir", "/etc/snapshot-provenance/tls", "Directory containing tls.crt and tls.key")
	reflector := flag.String("reflector-username", "", "Authenticated provider API username of the Liqo reflector")
	flag.Parse()
	if err := run(*bindingPath, *certDir, *reflector); err != nil {
		ctrl.Log.Error(err, "provenance webhook stopped")
		os.Exit(1)
	}
}

func run(bindingPath, certDir, reflector string) error {
	data, err := os.ReadFile(bindingPath)
	if err != nil {
		return err
	}
	binding := &remote.Binding{}
	if err := yaml.UnmarshalStrict(data, binding); err != nil {
		return err
	}
	if err := binding.Validate(); err != nil {
		return err
	}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	localConfig, err := ctrl.GetConfig()
	if err != nil {
		return err
	}
	local, err := client.New(localConfig, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}
	remoteConfig, err := remote.RESTConfig(binding.Kubeconfig)
	if err != nil {
		return err
	}
	origin, err := client.New(remoteConfig, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}
	validator := &provenance.ShadowValidator{Binding: binding, Origin: origin, Provider: local, ReflectorUsername: reflector}
	if err := validator.Validate(); err != nil {
		return err
	}
	server := webhook.NewServer(webhook.Options{Port: 9443, CertDir: certDir})
	server.Register("/validate-shadowpod", &admission.Webhook{Handler: validator})
	mgr, err := ctrl.NewManager(localConfig, ctrl.Options{
		Scheme: scheme, WebhookServer: server, HealthProbeBindAddress: ":8081",
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		return err
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("readyz", server.StartedChecker()); err != nil {
		return err
	}
	return mgr.Start(ctrl.SetupSignalHandler())
}
