package sat

import (
	"crypto/rand"
	"flag"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/pkg/errors"
	"github.com/suborbital/atmo/atmo/appsource"
	"github.com/suborbital/atmo/atmo/coordinator/capabilities"
	"github.com/suborbital/atmo/atmo/options"
	"github.com/suborbital/atmo/directive"
	"github.com/suborbital/atmo/fqfn"
	"github.com/suborbital/reactr/rcap"
	"github.com/suborbital/vektor/vlog"
	"gopkg.in/yaml.v2"
)

var useStdin bool

func init() {
	flag.BoolVar(&useStdin, "stdin", false, "read stdin as input, return output to stdout and then terminate")
}

type Config struct {
	RunnableArg     string
	JobType         string
	PrettyName      string
	Runnable        *directive.Runnable
	Identifier      string
	CapConfig       rcap.CapabilityConfig
	Port            int
	UseStdin        bool
	ControlPlaneUrl string
	EnvToken        string
	Logger          *vlog.Logger
	ProcUUID        string
}

type satInfo struct {
	SatVersion string `json:"sat_version"`
}

type app struct {
	Name string `json:"name"`
}

func ConfigFromArgs() (*Config, error) {
	flag.Parse()
	args := flag.Args()

	if len(args) < 1 {
		return nil, errors.New("missing argument: runnable (path, URL or FQFN)")
	}

	runnableArg := args[0]

	return ConfigFromRunnableArg(runnableArg)
}

func ConfigFromRunnableArg(runnableArg string) (*Config, error) {
	logger := vlog.Default(
		vlog.EnvPrefix("SAT"),
		vlog.AppMeta(satInfo{SatVersion: SatDotVersion}),
	)

	var runnable *directive.Runnable

	// first, determine if we need to connect to a control plane
	controlPlane, useControlPlane := os.LookupEnv("SAT_CONTROL_PLANE")
	appClient := appsource.NewHTTPSource(controlPlane)
	caps := rcap.DefaultConfigWithLogger(logger)

	if useControlPlane {
		// configure the appSource not to wait if the controlPlane isn't available
		opts := options.Options{Logger: logger, Wait: &wait, Headless: &headless}

		if err := appClient.Start(opts); err != nil {
			return nil, errors.Wrap(err, "failed to appSource.Start")
		}
	}

	envToken := os.Getenv("SAT_ENV_TOKEN")

	// next, handle the runnable arg being a URL, an FQFN, or a path on disk
	if isURL(runnableArg) {
		logger.Debug("fetching module from URL")
		tmpFile, err := downloadFromURL(runnableArg)
		if err != nil {
			return nil, errors.Wrap(err, "failed to downloadFromURL")
		}

		runnableArg = tmpFile
	} else if FQFN := fqfn.Parse(runnableArg); FQFN.Identifier != "" {
		if useControlPlane {
			logger.Debug("fetching module from control plane")

			cpRunnable, err := appClient.FindRunnable(runnableArg, envToken)
			if err != nil {
				return nil, errors.Wrap(err, "failed to FindRunnable")
			}

			runnable = cpRunnable

			rendered, err := capabilities.ResolveFromSource(appClient, FQFN.Identifier, FQFN.Namespace, FQFN.Version, logger)
			if err != nil {
				return nil, errors.Wrap(err, "failed to capabilities.Render")
			}

			caps = rendered
		}
	} else {
		diskRunnable, err := findRunnableDotYaml(runnableArg)
		if err != nil {
			return nil, errors.Wrap(err, "failed to findRunnable")
		}

		if diskRunnable != nil {
			ident, iExists := os.LookupEnv("SAT_RUNNABLE_IDENT")
			version, vExists := os.LookupEnv("SAT_RUNNABLE_VERSION")
			if iExists && vExists {
				FQFN := fqfn.FromParts(ident, runnable.Namespace, runnable.Name, version)
				runnable.FQFN = FQFN
			}
		}

		runnable = diskRunnable
	}

	// next, figure out the configuration of the HTTP server
	port, ok := os.LookupEnv("SAT_HTTP_PORT")
	if !ok {
		// choose a random port above 1000
		randPort, err := rand.Int(rand.Reader, big.NewInt(10000))
		if err != nil {
			return nil, errors.Wrap(err, "failed to rand.Int")
		}

		port = fmt.Sprintf("%d", randPort.Int64()+1000)
	}

	procUUID, ok := os.LookupEnv("SAT_UUID")
	if !ok {
		procUUID = uuid.New().String()
	} else {
		if _, err := uuid.Parse(procUUID); err != nil {
			return nil, errors.Wrap(err, "SAT_UUID is set, but is not valid UUID")
		}
	}

	// set some defaults in the case we're not running in an application
	portInt, _ := strconv.Atoi(port)
	jobType := strings.TrimSuffix(filepath.Base(runnableArg), ".wasm")
	FQFN := fqfn.Parse(jobType)
	prettyName := jobType

	// modify configuration if we ARE running as part of an application
	if runnable != nil && runnable.FQFN != "" {
		jobType = runnable.FQFN
		FQFN = fqfn.Parse(runnable.FQFN)

		prettyName = fmt.Sprintf("%s-%s", jobType, procUUID[:6])

		// replace the logger with something more detailed
		logger = vlog.Default(
			vlog.EnvPrefix("SAT"),
			vlog.AppMeta(app{prettyName}),
		)

		logger.Info("configuring", jobType)
		logger.Info("joining app", FQFN.Identifier)
	} else {
		logger.Debug("configuring", jobType)
	}

	// finally, put it all together
	c := &Config{
		RunnableArg:     runnableArg,
		JobType:         jobType,
		PrettyName:      prettyName,
		Runnable:        runnable,
		Identifier:      FQFN.Identifier,
		CapConfig:       caps,
		Port:            portInt,
		UseStdin:        useStdin,
		ControlPlaneUrl: controlPlane,
		Logger:          logger,
		ProcUUID:        procUUID,
	}

	return c, nil
}

func findRunnableDotYaml(runnableArg string) (*directive.Runnable, error) {
	filename := filepath.Base(runnableArg)
	runnableFilepath := strings.Replace(runnableArg, filename, ".runnable.yml", -1)

	if _, err := os.Stat(runnableFilepath); err != nil {
		// .runnable.yaml doesn't exist, don't bother returning error
		return nil, nil
	}

	runnableBytes, err := os.ReadFile(runnableFilepath)
	if err != nil {
		return nil, errors.Wrap(err, "failed to ReadFile")
	}

	runnable := &directive.Runnable{}
	if err := yaml.Unmarshal(runnableBytes, runnable); err != nil {
		return nil, errors.Wrap(err, "failed to Unmarshal")
	}

	return runnable, nil
}

func randSuffix() (string, error) {
	alphabet := "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

	suffix := ""

	for i := 0; i < 6; i++ {
		index, err := rand.Int(rand.Reader, big.NewInt(35))
		if err != nil {
			return "", errors.Wrap(err, "failed to rand.Int")
		}

		suffix += string(alphabet[index.Int64()])
	}

	return suffix, nil
}
