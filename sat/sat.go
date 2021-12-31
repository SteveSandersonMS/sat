package sat

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/pkg/errors"
	"github.com/suborbital/atmo/atmo/coordinator/executor"
	"github.com/suborbital/atmo/atmo/coordinator/sequence"
	"github.com/suborbital/grav/discovery/local"
	"github.com/suborbital/grav/grav"
	"github.com/suborbital/grav/transport/websocket"
	"github.com/suborbital/reactr/request"
	"github.com/suborbital/reactr/rt"
	"github.com/suborbital/reactr/rwasm"
	"github.com/suborbital/reactr/rwasm/runtime"
	"github.com/suborbital/vektor/vk"
	"github.com/suborbital/vektor/vlog"
)

const (
	MsgTypeAtmoFnResult = "atmo.fnresult"
)

// sat is a sat server with annoyingly terse field names (because it's smol)
type Sat struct {
	j string // the job name / FQFN

	c *Config
	v *vk.Server
	g *grav.Grav
	t *websocket.Transport
	e *executor.Executor
	l *vlog.Logger
}

var wait bool = false
var headless bool = false

// New initializes Reactr, Vektor, and Grav in a Sat instance
// if config.UseStdin is true, only Reactr will be created
func New(config *Config) (*Sat, error) {
	runtime.UseInternalLogger(config.Logger)

	exec := executor.NewWithGrav(config.Logger, nil)
	exec.UseCapabilityConfig(config.CapConfig)

	var runner rt.Runnable
	if config.Runnable != nil && len(config.Runnable.ModuleRef.Data) > 0 {
		runner = rwasm.NewRunnerWithRef(config.Runnable.ModuleRef)
	} else {
		runner = rwasm.NewRunner(config.RunnableArg)
	}

	exec.Register(
		config.JobType,
		runner,
		rt.Autoscale(0),
		rt.MaxRetries(0),
		rt.RetrySeconds(0),
		rt.PreWarm(),
	)

	sat := &Sat{
		c: config,
		j: config.JobType,
		e: exec,
		t: websocket.New(),
		l: config.Logger,
	}

	// no need to continue setup if we're in stdin mode, so return here
	if config.UseStdin {
		return sat, nil
	}

	// Grav and Vektor will be started on call to s.Start()

	sat.v = vk.New(
		vk.UseLogger(config.Logger),
		vk.UseAppName(config.PrettyName),
		vk.UseHTTPPort(config.Port),
		vk.UseEnvPrefix("SAT"),
	)

	sat.v.HandleHTTP(http.MethodGet, "/meta/message", sat.t.HTTPHandlerFunc())
	sat.v.GET("/meta/metrics", sat.metricsHandler())
	sat.v.POST("/*any", sat.handler(exec))

	return sat, nil
}

// Start starts Sat's Vektor server and Grav discovery
func (s *Sat) Start() error {
	errChan := make(chan error)

	// start Vektor first so that the server is started up before Grav starts discovery
	go func() {
		errChan <- s.v.Start()
	}()

	// configure Grav to join the mesh for its appropriate application
	// and broadcast its capability (i.e. the loaded function)
	s.g = grav.New(
		grav.UseBelongsTo(s.c.Identifier),
		grav.UseCapabilities(s.c.JobType),
		grav.UseLogger(s.c.Logger),
		grav.UseTransport(s.t),
		grav.UseDiscovery(local.New()),
		grav.UseEndpoint(fmt.Sprintf("%d", s.c.Port), "/meta/message"),
	)

	// set up the Executor to listen for jobs and handle them
	s.e.UseGrav(s.g)
	s.e.ListenAndRun(s.c.JobType, s.handleFnResult)

	if err := connectStaticPeers(s.c.Logger, s.g); err != nil {
		log.Fatal(err)
	}

	return <-errChan
}

// execFromStdin reads stdin, passes the data through the registered module, and writes the result to stdout
func (s *Sat) ExecFromStdin() error {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Scan()

	if err := scanner.Err(); err != nil {
		return errors.Wrap(err, "failed to scanner.Scan")
	}

	input := scanner.Bytes()

	ctx := vk.NewCtx(s.l, nil, nil)

	// construct a fake HTTP request from the input
	req := &request.CoordinatedRequest{
		Method:      http.MethodPost,
		URL:         "/",
		ID:          ctx.RequestID(),
		Body:        input,
		Headers:     map[string]string{},
		RespHeaders: map[string]string{},
		Params:      map[string]string{},
		State:       map[string][]byte{},
	}

	result, err := s.e.Do(s.j, req, ctx)
	if err != nil {
		return errors.Wrap(err, "failed to exec")
	}

	resp := request.CoordinatedResponse{}
	if err := json.Unmarshal(result.([]byte), &resp); err != nil {
		return errors.Wrap(err, "failed to Unmarshal response")
	}

	fmt.Print(string(resp.Output))

	return nil
}

func (s *Sat) handler(exec *executor.Executor) vk.HandlerFunc {
	return func(r *http.Request, ctx *vk.Ctx) (interface{}, error) {
		req, err := request.FromVKRequest(r, ctx)
		if err != nil {
			ctx.Log.Error(errors.Wrap(err, "failed to FromVKRequest"))
			return nil, vk.E(http.StatusInternalServerError, "unknown error")
		}

		result, err := exec.Do(s.j, req, ctx)
		if err != nil {
			ctx.Log.Error(errors.Wrap(err, "failed to exec"))
			return nil, vk.Wrap(http.StatusTeapot, err)
		}

		resp := request.CoordinatedResponse{}
		if err := json.Unmarshal(result.([]byte), &resp); err != nil {
			ctx.Log.Error(errors.Wrap(err, "failed to Unmarshal resp"))
			return nil, vk.E(http.StatusInternalServerError, "unknown error")
		}

		return resp.Output, nil
	}
}

// handleFnResult this is the function mounted onto exec.ListenAndRun, and receives all
// function results received from meshed peers (i.e. Grav)
func (s *Sat) handleFnResult(msg grav.Message, result interface{}, fnErr error) {
	s.l.Info(msg.Type(), "finished executing")

	// first unmarshal the request and sequence information
	req, err := request.FromJSON(msg.Data())
	if err != nil {
		s.l.Error(errors.Wrap(err, "failed to request.FromJSON"))
		return
	}

	ctx := vk.NewCtx(s.l, nil, nil)
	ctx.UseRequestID(req.ID)

	seq, err := sequence.FromJSON(req.SequenceJSON, req, s.e, ctx)
	if err != nil {
		s.l.Error(errors.Wrap(err, "failed to sequence.FromJSON"))
		return
	}

	// figure out where we are in the sequence
	step := seq.NextStep()
	if step == nil {
		s.l.Error(errors.New("got nil NextStep"))
		return
	}

	step.Completed = true

	// start evaluating the result of the function call
	resp := &request.CoordinatedResponse{}
	var runErr rt.RunErr
	var execErr error

	if fnErr != nil {
		if fnRunErr, isRunErr := fnErr.(rt.RunErr); isRunErr {
			// great, it's a runErr
			runErr = fnRunErr
		} else {
			execErr = fnErr
		}
	} else {
		respJSON := result.([]byte)
		if err := json.Unmarshal(respJSON, resp); err != nil {
			s.l.Error(errors.Wrap(err, "failed to Unmarshal response"))
			return
		}
	}

	// package everything up and shuttle it back to the parent (atmo-proxy)
	fnr := &sequence.FnResult{
		FQFN:     msg.Type(),
		Key:      step.Exec.CallableFn.Key(), // to support groups, we'll need to find the correct CallableFn in the list
		Response: resp,
		RunErr:   runErr,
		ExecErr: func() string {
			if execErr != nil {
				return execErr.Error()
			}

			return ""
		}(),
	}

	pod := s.g.Connect()
	defer pod.Disconnect()

	if err := s.sendFnResult(pod, fnr, ctx); err != nil {
		s.l.Error(errors.Wrap(err, "failed to sendFnResult"))
		return
	}

	// determine if we ourselves should continue or halt the sequence
	if execErr != nil {
		s.l.ErrorString("stopping execution after error failed execution of", msg.Type(), ":", execErr.Error())
		return
	}

	if err := seq.HandleStepErrs([]sequence.FnResult{*fnr}, step.Exec); err != nil {
		s.l.Error(err)
		return
	}

	// load the results into the request state
	seq.HandleStepResults([]sequence.FnResult{*fnr})

	// prepare for the next step in the chain to be executed
	stepJSON, err := seq.StepsJSON()
	if err != nil {
		s.l.Error(errors.Wrap(err, "failed to StepsJSON"))
		return
	}

	req.SequenceJSON = stepJSON

	s.sendNextStep(pod, msg, seq, req)
}

func (s *Sat) sendFnResult(pod *grav.Pod, result *sequence.FnResult, ctx *vk.Ctx) error {
	fnrJSON, err := json.Marshal(result)
	if err != nil {
		return errors.Wrap(err, "failed to Marshal function result")
	}

	s.l.Info("function", s.j, "completed, sending result")

	respMsg := grav.NewMsgWithParentID(MsgTypeAtmoFnResult, ctx.RequestID(), fnrJSON)
	pod.Send(respMsg)

	return nil
}

func (s *Sat) sendNextStep(pod *grav.Pod, msg grav.Message, seq *sequence.Sequence, req *request.CoordinatedRequest) {
	nextStep := seq.NextStep()
	if nextStep == nil {
		s.l.Info("sequence completed, no nextStep message to send")
		return
	}

	reqJSON, err := json.Marshal(req)
	if err != nil {
		s.l.Error(errors.Wrap(err, "failed to Marshal request"))
		return
	}

	s.l.Info("sending next message", nextStep.Exec.FQFN)

	nextMsg := grav.NewMsgWithParentID(nextStep.Exec.FQFN, msg.ParentID(), reqJSON)
	s.g.Tunnel(nextStep.Exec.FQFN, nextMsg)
}
