package lima

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/abiosoft/colima/cli"
	"github.com/abiosoft/colima/config"
	"github.com/abiosoft/colima/config/configmanager"
	"github.com/abiosoft/colima/core"
	"github.com/abiosoft/colima/daemon"
	"github.com/abiosoft/colima/environment"
	"github.com/abiosoft/colima/environment/container/containerd"
	"github.com/abiosoft/colima/environment/vm/lima/limautil"
	"github.com/abiosoft/colima/util"
	"github.com/abiosoft/colima/util/osutil"
	"github.com/abiosoft/colima/util/yamlutil"
	"github.com/sirupsen/logrus"
)

// New creates a new virtual machine.
func New(host environment.HostActions) environment.VM {
	// lima config directory
	limaHome := limautil.LimaHome()

	// environment variables for the subprocesses
	var envs []string
	envHome := limautil.EnvLimaHome + "=" + limaHome
	envLimaInstance := envLimaInstance + "=" + config.CurrentProfile().ID
	envBinary := osutil.EnvColimaBinary + "=" + osutil.Executable()
	envs = append(envs, envHome, envLimaInstance, envBinary)

	// consider making this truly flexible to support other VMs
	return &limaVM{
		host:         host.WithEnv(envs...),
		limaHome:     limaHome,
		CommandChain: cli.New("vm"),
		daemon:       daemon.NewManager(host),
	}
}

const (
	envLimaInstance = "LIMA_INSTANCE"
	lima            = "lima"
	limactl         = limautil.LimactlCommand
)

func (l limaVM) limaConfFile() string {
	return filepath.Join(l.limaHome, config.CurrentProfile().ID, "lima.yaml")
}

var _ environment.VM = (*limaVM)(nil)

type limaVM struct {
	host environment.HostActions
	cli.CommandChain

	// keep config in case of restart
	conf config.Config

	// lima config
	limaConf Config

	// lima config directory
	limaHome string

	// network between host and the vm
	daemon daemon.Manager
}

func (l limaVM) Dependencies() []string {
	return []string{
		"lima",
	}
}

func (l *limaVM) Start(ctx context.Context, conf config.Config) error {
	a := l.Init(ctx)

	if l.Created() {
		return l.resume(ctx, conf)
	}

	a.Add(func() (err error) {
		ctx, err = l.startDaemon(ctx, conf)
		return err
	})

	a.Stage("creating and starting")
	configFile := filepath.Join(os.TempDir(), config.CurrentProfile().ID+".yaml")

	a.Add(func() (err error) {
		l.limaConf, err = newConf(ctx, conf)
		if err != nil {
			return err
		}
		return yamlutil.WriteYAML(l.limaConf, configFile)
	})
	a.Add(l.writeNetworkFile)
	a.Add(func() error {
		return l.host.Run(limactl, "start", "--tty=false", configFile)
	})
	a.Add(func() error {
		return os.Remove(configFile)
	})

	// adding it to command chain to execute only after successful startup.
	a.Add(func() error {
		l.conf = conf
		return nil
	})

	l.addPostStartActions(a, conf)

	return a.Exec()
}

func (l *limaVM) resume(ctx context.Context, conf config.Config) error {
	log := l.Logger(ctx)
	a := l.Init(ctx)

	if l.Running(ctx) {
		log.Println("already running")
		return nil
	}

	a.Add(func() (err error) {
		ctx, err = l.startDaemon(ctx, conf)
		return err
	})

	a.Add(func() (err error) {
		// disk must be resized before starting
		conf = l.syncDiskSize(ctx, conf)

		l.limaConf, err = newConf(ctx, conf)
		if err != nil {
			return err
		}
		return yamlutil.WriteYAML(l.limaConf, l.limaConfFile())
	})

	a.Add(l.writeNetworkFile)

	a.Stage("starting")
	a.Add(func() error {
		return l.host.Run(limactl, "start", config.CurrentProfile().ID)
	})

	l.addPostStartActions(a, conf)

	return a.Exec()
}

func (l limaVM) Running(_ context.Context) bool {
	i, err := limautil.Instance()
	if err != nil {
		logrus.Trace(fmt.Errorf("error retrieving running instance: %w", err))
		return false
	}
	return i.Running()
}

func (l limaVM) Stop(ctx context.Context, force bool) error {
	log := l.Logger(ctx)
	a := l.Init(ctx)
	if !l.Running(ctx) && !force {
		log.Println("not running")
		return nil
	}

	a.Stage("stopping")

	if util.MacOS() {
		conf, _ := limautil.InstanceConfig()
		a.Retry("", time.Second*1, 10, func(retryCount int) error {
			err := l.daemon.Stop(ctx, conf)
			if err != nil {
				err = cli.ErrNonFatal(err)
			}
			return err
		})
	}

	a.Add(func() error {
		if force {
			return l.host.Run(limactl, "stop", "--force", config.CurrentProfile().ID)
		}
		return l.host.Run(limactl, "stop", config.CurrentProfile().ID)
	})

	return a.Exec()
}

func (l limaVM) Teardown(ctx context.Context) error {
	a := l.Init(ctx)

	if util.MacOS() {
		conf, _ := limautil.InstanceConfig()
		a.Retry("", time.Second*1, 10, func(retryCount int) error {
			return l.daemon.Stop(ctx, conf)
		})
	}

	a.Add(func() error {
		return l.host.Run(limactl, "delete", "--force", config.CurrentProfile().ID)
	})

	return a.Exec()
}

func (l limaVM) Restart(ctx context.Context) error {
	if l.conf.Empty() {
		return fmt.Errorf("cannot restart, VM not previously started")
	}

	if err := l.Stop(ctx, false); err != nil {
		return err
	}

	// minor delay to prevent possible race condition.
	time.Sleep(time.Second * 2)

	if err := l.Start(ctx, l.conf); err != nil {
		return err
	}

	return nil
}

func (l limaVM) Host() environment.HostActions {
	return l.host
}

func (l limaVM) Env(s string) (string, error) {
	ctx := context.Background()
	if !l.Running(ctx) {
		return "", fmt.Errorf("not running")
	}
	return l.RunOutput("echo", "$"+s)
}

func (l limaVM) Created() bool {
	stat, err := os.Stat(l.limaConfFile())
	return err == nil && !stat.IsDir()
}

func (l limaVM) User() (string, error) {
	return l.RunOutput("whoami")
}

func (l limaVM) Arch() environment.Arch {
	a, _ := l.RunOutput("uname", "-m")
	return environment.Arch(a)
}

func (l *limaVM) syncDiskSize(ctx context.Context, conf config.Config) config.Config {
	log := l.Logger(ctx)
	instance, err := limautil.InstanceConfig()
	if err != nil {
		// instance config missing, ignore
		return conf
	}

	resized := func() bool {
		if instance.Disk == conf.Disk {
			// nothing to do
			return false
		}

		if conf.VMType == VZ {
			log.Warnln("dynamic disk resize not supported for VZ driver, ignoring...")
			return false
		}

		size := conf.Disk - instance.Disk
		if size < 0 {
			log.Warnln("disk size cannot be reduced, ignoring...")
			return false
		}

		sizeStr := fmt.Sprintf("%dG", conf.Disk)
		args := []string{"qemu-img", "resize"}
		disk := limautil.ColimaDiffDisk(config.CurrentProfile().ID)
		args = append(args, disk, sizeStr)

		// qemu-img resize /path/to/diffdisk +10G
		if err := l.host.RunQuiet(args...); err != nil {
			log.Warnln(fmt.Errorf("unable to resize disk: %w", err))
			return false
		}

		log.Printf("resizing disk to %dGiB...", conf.Disk)
		return true
	}()

	if !resized {
		conf.Disk = instance.Disk
	}

	return conf
}

func (l *limaVM) addPostStartActions(a *cli.ActiveCommandChain, conf config.Config) {
	// package dependencies
	a.Add(func() error {
		return l.installDependencies(a.Logger(), conf)
	})

	// containerd dependencies
	if conf.Runtime == containerd.Name {
		a.Add(func() error {
			return core.SetupContainerdUtils(l.host, l, environment.Arch(conf.Arch))
		})
	}

	// registry certs
	a.Add(l.copyCerts)

	// cross-platform emulation
	a.Add(func() error {
		if !l.limaConf.Rosetta.Enabled {
			// use binfmt when rosetta is disabled and emulation is disabled i.e. host arch
			if arch := environment.HostArch(); arch == environment.Arch(conf.Arch).Value() {
				if err := core.SetupBinfmt(l.host, l, environment.Arch(conf.Arch)); err != nil {
					logrus.Warn(fmt.Errorf("unable to enable qemu %s emulation: %w", arch, err))
				}
			}

			// rosetta is disabled
			return nil
		}

		// enable rosetta
		err := l.Run("sudo", "sh", "-c", `stat /proc/sys/fs/binfmt_misc/rosetta || echo ':rosetta:M::\x7fELF\x02\x01\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x02\x00\x3e\x00:\xff\xff\xff\xff\xff\xfe\xfe\x00\xff\xff\xff\xff\xff\xff\xff\xff\xfe\xff\xff\xff:/mnt/lima-rosetta/rosetta:OCF' > /proc/sys/fs/binfmt_misc/register`)
		if err != nil {
			logrus.Warn(fmt.Errorf("unable to enable rosetta: %w", err))
			return nil
		}

		// disable qemu
		if err := l.RunQuiet("stat", "/proc/sys/fs/binfmt_misc/qemu-x86_64"); err == nil {
			err = l.Run("sudo", "sh", "-c", `echo 0 > /proc/sys/fs/binfmt_misc/qemu-x86_64`)
			if err != nil {
				logrus.Warn(fmt.Errorf("unable to disable qemu x86_84 emulation: %w", err))
			}
		}

		return nil
	})

	// preserve state
	a.Add(func() error {
		if err := configmanager.SaveToFile(conf, limautil.ColimaStateFile(config.CurrentProfile().ID)); err != nil {
			logrus.Warnln(fmt.Errorf("error persisting Colima state: %w", err))
		}
		return nil
	})
}
