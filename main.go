package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"net/url"
	"os"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/grafana/pyroscope-go"
	pyroscope_pprof "github.com/grafana/pyroscope-go/http/pprof"
	"github.com/luthermonson/go-proxmox"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

func main() {

	pyroscopeAddr := os.Getenv("PYROSCOPE_ADDR")

	pyro, err := pyroscope.Start(pyroscope.Config{
		ApplicationName: "proxmox-k8s-sync",
		ServerAddress:   pyroscopeAddr,
		ProfileTypes: []pyroscope.ProfileType{
			pyroscope.ProfileCPU,
			pyroscope.ProfileInuseObjects,
			pyroscope.ProfileAllocObjects,
			pyroscope.ProfileInuseSpace,
			pyroscope.ProfileAllocSpace,
			pyroscope.ProfileGoroutines,
			pyroscope.ProfileMutexCount,
			pyroscope.ProfileMutexDuration,
			pyroscope.ProfileBlockCount,
			pyroscope.ProfileBlockDuration,
		},
	})
	if err != nil {
		slog.Warn("failed to connect to profiler", slog.String("error", err.Error()))
	}
	defer func() {
		err := pyro.Stop()
		if err != nil {
			slog.Error("Stopped profiling", slog.String("error", err.Error()))
		}
	}()

	username := os.Getenv("PROXMOX_USERNAME")
	password := os.Getenv("PROXMOX_PASSWORD")
	token := os.Getenv("PROXMOX_TOKEN_ID")
	tokenSecret := os.Getenv("PROXMOX_TOKEN_SECRET")

	var proxmoxClientOpts []proxmox.Option

	if token != "" && tokenSecret != "" {
		proxmoxClientOpts = append(proxmoxClientOpts, proxmox.WithAPIToken(token, tokenSecret))
	} else if username != "" && password != "" {
		credentials := proxmox.Credentials{
			Username: username,
			Password: password,
		}
		proxmoxClientOpts = append(proxmoxClientOpts, proxmox.WithCredentials(&credentials))
	} else {
		slog.Error("Could not establish connection options for proxmox")
		os.Exit(1)
	}

	proxmoxUrl := os.Getenv("PROXMOX_URL")
	if !strings.Contains(proxmoxUrl, "/api2/json") {
		slog.Warn("/api2/json not found, attempting to add")
		proxmoxUrl, err = url.JoinPath(proxmoxUrl, "/api2/json")
		if err != nil {
			slog.Error("Failed to join path", slog.String("URL", proxmoxUrl), slog.String("Error", err.Error()))
			os.Exit(1)
		}
	}

	slog.Info("Attempting to connect to proxmox", slog.String("URL", proxmoxUrl))

	client := proxmox.NewClient(proxmoxUrl, proxmoxClientOpts...)

	version, err := client.Version(context.TODO())
	if err != nil {
		panic(err.Error())
	}

	slog.Info("Connected to Proxmox", slog.String("URL", proxmoxUrl), slog.String("Version", version.Release))

	config, err := rest.InClusterConfig()

	// If we don't detect that we are running in a cluster, attempt to use the .kube/config file
	if err != nil {
		kubeconfig := path.Join(homedir.HomeDir(), ".kube", "config")
		if _, err := os.Stat(kubeconfig); errors.Is(err, fs.ErrNotExist) {
			slog.Error("kubeconfig not found", slog.String("Path", kubeconfig))
			os.Exit(1)
		}

		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			slog.Error("Error building config", slog.String("Path", kubeconfig), slog.String("Error", err.Error()))
			os.Exit(1)
		}
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		slog.Error("Error creating clientset", slog.String("Error", err.Error()))
		os.Exit(1)
	}

	traceAddr := os.Getenv("OTEL_ENDPOINT")
	if traceAddr != "" {
		ctx, shutdown, err := InitializeTracer(traceAddr)
		if err != nil {
			slog.Error("Failed to initialize tracer", slog.String("Error", err.Error()))
			os.Exit(1)
		}
		defer func() {
			err := shutdown(ctx)
			if err != nil {
				slog.Error("Failed to shutdown tracer", slog.String("Error", err.Error()))
				os.Exit(1)
			}
		}()
	}

	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/debug/pprof", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
		mux.HandleFunc("/debug/pprof/profile", pyroscope_pprof.Profile)

		mux.Handle("/metrics", promhttp.Handler())

		mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
			_, span := StartTrace(context.TODO(), "readyz")
			defer span.End()
			w.WriteHeader(200)
			span.SetStatus(codes.Ok, "completed call")
		})
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
			_, span := StartTrace(context.TODO(), "healthz")
			defer span.End()
			w.WriteHeader(200)
			span.SetStatus(codes.Ok, "completed call")
		})
		err := http.ListenAndServe(":8080", mux)
		if err != nil {
			slog.Error("Error starting healthcheck server", slog.String("Error", err.Error()))
			os.Exit(1)
		}
	}()
	for {
		err := SyncNodes(clientset, client, proxmoxUrl)
		if err != nil {
			slog.Error("Failed to sync nodes", slog.String("Error", err.Error()))
		}

		time.Sleep(5 * time.Minute)
	}
}

func SyncNodes(clientset *kubernetes.Clientset, client *proxmox.Client, proxmoxUrl string) error {

	ctx, span := StartTrace(context.Background(), "")
	defer span.End()

	nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})

	if err != nil {
		return TraceError(ctx, span, fmt.Errorf("failed to get nodes from kubernetes: %w", err))
	}
	slog.Info("Found nodes in cluster", slog.Int("count", len(nodes.Items)))
	ipHostnameMap := make(map[string]string)

	for _, n := range nodes.Items {
		internalIPIndex := slices.IndexFunc(n.Status.Addresses, func(a corev1.NodeAddress) bool { return a.Type == corev1.NodeInternalIP })
		slog.Debug(
			"Kubernetes Node details",
			slog.String("hostname", n.Name),
			slog.String("ip", n.Status.Addresses[internalIPIndex].Address),
		)
		ipHostnameMap[n.Status.Addresses[internalIPIndex].Address] = n.Name
	}

	proxmoxNodes, err := client.Nodes(ctx)
	if err != nil {
		return TraceError(ctx, span, fmt.Errorf("failed to get nodes from proxmox: %w", err))
	}

	for _, n := range proxmoxNodes {
		err = SyncNode(ctx, n, client, ipHostnameMap)
		if err != nil {
			_ = TraceError(ctx, span, fmt.Errorf("failed to sync node: %w", err))
		}
	}
	slog.Info("Completed update cycle")
	return nil
}

func SyncNode(ctx context.Context, n *proxmox.NodeStatus, client *proxmox.Client, ipHostnameMap map[string]string) error {
	ctx, span := StartTrace(ctx, "", trace.WithAttributes(attribute.String("node", n.ID)))
	defer span.End()


	slog.Info("Updating Proxmox node vms", slog.String("Name", n.ID))
	proxmoxNode, err := client.Node(ctx, strings.Split(n.ID, "/")[1])
	if err != nil {
		return TraceError(ctx, span, fmt.Errorf("failed to get node from proxmox: %w", err))
	}
	vms, err := proxmoxNode.VirtualMachines(ctx)
	if err != nil {
		return TraceError(ctx, span, fmt.Errorf("failed to get vms from proxmox: %w", err))
	}
	for _, vm := range vms {
		ifs, err := vm.AgentGetNetworkIFaces(ctx)
		if err != nil {
			if strings.Contains(err.Error(), "is not running") {
				span.AddEvent("skipping vm as it is not running")
				continue
			}
			if err.Error() == "500 No QEMU guest agent configured" {
				span.AddEvent("skipping vm as it has no QEMU guest agent configured")
				continue
			}

			_ = TraceError(ctx, span, fmt.Errorf("failed to get guest info from proxmox: %w", err))
			continue
		}

		for _, i := range ifs {
			for _, ip := range i.IPAddresses {
				if ip == nil {
					continue
				}
				if ip.IPAddressType == "ipv6" {
					continue
				}
				hostname, ok := ipHostnameMap[ip.IPAddress]
				if !ok {
					continue
				}
				if hostname == vm.Name {
					span.AddEvent("name already matching", trace.WithAttributes(attribute.String("hostname", hostname), attribute.String("IP", ip.IPAddress), attribute.String("name", vm.Name)))
					continue
				}
				span.AddEvent("setting IP Address", trace.WithAttributes(attribute.String("IP", ip.IPAddress), attribute.String("Type", ip.IPAddressType), attribute.String("Hostname", hostname), attribute.String("name", vm.Name)))
				_, err := vm.Config(ctx, proxmox.VirtualMachineOption{Name: "name", Value: hostname})
				if err != nil {
					_ = TraceError(ctx, span, fmt.Errorf("failed to set vm name in proxmox: %w", err))
				}
			}
		}
	}
	return nil
}
