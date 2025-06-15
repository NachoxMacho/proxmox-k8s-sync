package main

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/luthermonson/go-proxmox"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

func main() {

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

	var err error
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
	for {
		nodes, err := clientset.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})

		if err != nil {
			panic(err.Error())
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

		proxmoxNodes, err := client.Nodes(context.TODO())
		if err != nil {
			slog.Error("Failed to get nodes from proxmox", slog.String("URL", proxmoxUrl), slog.String("Error", err.Error()))
			continue
		}

		for _, n := range proxmoxNodes {
			slog.Info("Updating Proxmox node vms", slog.String("Name", n.ID))
			proxmoxNode, err := client.Node(context.TODO(), strings.Split(n.ID, "/")[1])
			if err != nil {
				slog.Error("Failed to get node from proxmox", slog.String("URL", proxmoxUrl), slog.String("Error", err.Error()))
				continue
			}
			vms, err := proxmoxNode.VirtualMachines(context.TODO())
			if err != nil {
				slog.Error("Failed to get vms from proxmox", slog.String("URL", proxmoxUrl), slog.String("Error", err.Error()))
			}
			for _, vm := range vms {
				ifs, err := vm.AgentGetNetworkIFaces(context.TODO())
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
							slog.Debug("Name already matching", slog.String("Hostname", hostname), slog.String("IP", ip.IPAddress), slog.String("name", vm.Name))
							continue
						}
						slog.Info("Setting IP Address", slog.String("IP", ip.IPAddress), slog.String("Type", ip.IPAddressType), slog.String("Hostname", hostname), slog.String("name", vm.Name))
						_, err := vm.Config(context.TODO(), proxmox.VirtualMachineOption{Name: "name", Value: hostname})
						if err != nil {
							slog.Error("Failed to get config from proxmox", slog.String("URL", proxmoxUrl), slog.String("Error", err.Error()))
						}
					}
				}

				if err != nil {
					if strings.Contains(err.Error(), "is not running") {
						slog.Debug("Skipping VM as it is not running", slog.String("Name", vm.Name))
						continue
					}
					if err.Error() == "500 No QEMU guest agent configured" {
						slog.Debug("Skipping VM as it has no QEMU guest agent configured", slog.String("Name", vm.Name))
						continue
					}

					slog.Error("Failed to get guest info from proxmox", slog.String("URL", proxmoxUrl), slog.String("Error", err.Error()))
				}
			}
		}

		slog.Info("Completed update cycle")

		time.Sleep(5 * time.Minute)
	}
}
