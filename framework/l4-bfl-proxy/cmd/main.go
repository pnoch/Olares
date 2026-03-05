package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/beclab/l4-bfl-proxy/internal/envoy"
	"github.com/beclab/l4-bfl-proxy/internal/message"
	"github.com/beclab/l4-bfl-proxy/internal/provider"
	"github.com/beclab/l4-bfl-proxy/internal/runner"
	"github.com/beclab/l4-bfl-proxy/internal/translator"
	"github.com/beclab/l4-bfl-proxy/internal/xds/server"
	xdstranslator "github.com/beclab/l4-bfl-proxy/internal/xds/translator"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
)

const (
	userNamespacePrefix = "user-space"
	sslServerPort       = 443
	sslProxyServerPort  = 444
	bflServicePort      = 444
	xdsServerPort       = 8794
)

func main() {
	klog.InitFlags(nil)
	flag.Parse()
	defer klog.Flush()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	config, err := getKubeConfig()
	if err != nil {
		klog.Fatalf("get kubeconfig: %v", err)
	}

	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		klog.Fatalf("create dynamic client: %v", err)
	}

	providerResources := &message.ProviderResources{}
	xdsIR := &message.XdsIR{}
	xdsResources := &message.XdsResources{}

	runners := []runner.Runner{
		provider.New(dynamicClient, providerResources, &provider.Config{
			UserNamespacePrefix: userNamespacePrefix,
			BFLServicePort:      bflServicePort,
			SSLServerPort:       sslServerPort,
			SSLProxyServerPort:  sslProxyServerPort,
		}),
		translator.New(providerResources, xdsIR, &translator.Config{
			SSLServerPort:       sslServerPort,
			SSLProxyServerPort:  sslProxyServerPort,
			UserNamespacePrefix: userNamespacePrefix,
		}),
		xdstranslator.New(xdsIR, xdsResources),
		server.New(xdsResources, &server.Config{
			Address: "127.0.0.1",
			Port:    xdsServerPort,
		}),
	}

	bootstrapCfg := envoy.DefaultBootstrapConfig(xdsServerPort)
	envoyCfg := envoy.DefaultEnvoyConfig()

	if err := envoy.WriteBootstrapConfig(envoyCfg.BootstrapPath, bootstrapCfg); err != nil {
		klog.Fatalf("write envoy bootstrap: %v", err)
	}

	if err := envoy.StartEnvoy(ctx, envoyCfg); err != nil {
		klog.Fatalf("start envoy: %v", err)
	}

	var wg sync.WaitGroup
	for _, r := range runners {
		wg.Add(1)
		go func(r runner.Runner) {
			defer wg.Done()
			klog.Infof("starting runner: %s", r.Name())
			if err := r.Start(ctx); err != nil {
				klog.Errorf("runner %s failed: %v", r.Name(), err)
				cancel()
			}
		}(r)
	}

	wg.Wait()
	klog.Info("all runners stopped")
}

func getKubeConfig() (*rest.Config, error) {
	cfg, err := rest.InClusterConfig()
	if err == nil {
		return cfg, nil
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, nil).ClientConfig()
}
