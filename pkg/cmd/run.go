package cmd

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/coreos/go-systemd/daemon"
	"github.com/openshift/microshift/pkg/config"
	"github.com/openshift/microshift/pkg/controllers"
	"github.com/openshift/microshift/pkg/kustomize"
	"github.com/openshift/microshift/pkg/mdns"
	"github.com/openshift/microshift/pkg/node"
	"github.com/openshift/microshift/pkg/servicemanager"
	"github.com/openshift/microshift/pkg/util"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"k8s.io/klog/v2"
)

const (
	gracefulShutdownTimeout = 60
)

func NewRunMicroshiftCommand() *cobra.Command {
	cfg := config.NewMicroshiftConfig()

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run MicroShift",
		RunE: func(cmd *cobra.Command, args []string) error {
			return RunMicroshift(cfg, cmd.Flags())
		},
	}

	flags := cmd.Flags()
	// Read the config flag directly into the struct, so it's immediately available.
	flags.StringVar(&cfg.ConfigFile, "config", cfg.ConfigFile, "File to read configuration from.")
	cmd.MarkFlagFilename("config", "yaml", "yml")
	// All other flags will be read after reading both config file and env vars.
	flags.String("data-dir", cfg.DataDir, "Directory for storing runtime data.")
	flags.String("audit-log-dir", cfg.AuditLogDir, "Directory for storing audit logs.")
	flags.StringSlice("roles", cfg.Roles, "Roles of this MicroShift instance.")

	return cmd
}

func RunMicroshift(cfg *config.MicroshiftConfig, flags *pflag.FlagSet) error {

	if err := cfg.ReadAndValidate(flags); err != nil {
		logrus.Fatal(err)
	}

	// fail early if we don't have enough privileges
	if config.StringInList("node", cfg.Roles) && os.Geteuid() > 0 {
		logrus.Fatalf("MicroShift must be run privileged for role 'node'")
	}

	// TO-DO: When multi-node is ready, we need to add the controller host-name/mDNS hostname
	//        or VIP to this list on start
	//        see https://github.com/redhat-et/microshift/pull/471

	if err := util.AddToNoProxyEnv(
		cfg.NodeIP,
		cfg.NodeName,
		cfg.Cluster.ClusterCIDR,
		cfg.Cluster.ServiceCIDR,
		".svc",
		"."+cfg.Cluster.Domain); err != nil {
		klog.Fatal(err)
	}

	os.MkdirAll(cfg.DataDir, 0700)
	os.MkdirAll(cfg.AuditLogDir, 0700)

	// TODO: change to only initialize what is strictly necessary for the selected role(s)
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "certs")); errors.Is(err, os.ErrNotExist) {
		initAll(cfg)
	} else {
		err = loadCA(cfg)
		if err != nil {
			err := os.RemoveAll(filepath.Join(cfg.DataDir, "certs"))
			if err != nil {
				klog.ErrorS(err, "removing old certs directory")
			}
			util.Must(initAll(cfg))
		}
	}

	m := servicemanager.NewServiceManager()
	if config.StringInList("controlplane", cfg.Roles) {
		util.Must(m.AddService(controllers.NewEtcd(cfg)))
		util.Must(m.AddService(controllers.NewKubeAPIServer(cfg)))
		util.Must(m.AddService(controllers.NewKubeScheduler(cfg)))
		util.Must(m.AddService(controllers.NewKubeControllerManager(cfg)))
		util.Must(m.AddService(controllers.NewOpenShiftControllerManager(cfg)))
		util.Must(m.AddService(controllers.NewOpenShiftCRDManager(cfg)))
		util.Must(m.AddService(controllers.NewOpenShiftAPIServer(cfg)))
		util.Must(m.AddService(controllers.NewOpenShiftOAuth(cfg)))

		util.Must(m.AddService(controllers.NewOpenShiftDefaultSCCManager(cfg)))
		util.Must(m.AddService(mdns.NewMicroShiftmDNSController(cfg)))
		util.Must(m.AddService(controllers.NewInfrastructureServices(cfg)))
		util.Must(m.AddService(kustomize.NewKustomizer(cfg)))
	}

	if config.StringInList("node", cfg.Roles) {
		util.Must(m.AddService(node.NewKubeletServer(cfg)))
		util.Must(m.AddService(node.NewKubeProxyServer(cfg)))
	}

	logrus.Info("Starting MicroShift")

	ctx, cancel := context.WithCancel(context.Background())
	ready, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		logrus.Infof("Starting %s", m.Name())
		if err := m.Run(ctx, ready, stopped); err != nil {
			logrus.Infof("%s stopped: %s", m.Name(), err)
		} else {
			logrus.Infof("%s completed", m.Name())
		}
	}()

	sigTerm := make(chan os.Signal, 1)
	signal.Notify(sigTerm, os.Interrupt, syscall.SIGTERM)

	select {
	case <-ready:
		logrus.Info("MicroShift is ready.")
		daemon.SdNotify(false, daemon.SdNotifyReady)

		<-sigTerm
	case <-sigTerm:
	}
	logrus.Info("Interrupt received. Stopping services.")
	cancel()

	select {
	case <-stopped:
	case <-sigTerm:
		logrus.Info("Another interrupt received. Force terminating services.")
	case <-time.After(time.Duration(gracefulShutdownTimeout) * time.Second):
		logrus.Info("Timed out waiting for services to stop.")
	}
	logrus.Info("MicroShift stopped.")
	return nil
}
