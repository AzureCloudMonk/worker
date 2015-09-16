package worker

import (
	"fmt"
	"time"

	gocontext "golang.org/x/net/context"

	"github.com/mitchellh/multistep"
	"github.com/travis-ci/worker/backend"
	"github.com/travis-ci/worker/context"
)

type stepOpenLogWriter struct {
	logTimeout   time.Duration
	maxLogLength int
}

func (s *stepOpenLogWriter) Run(state multistep.StateBag) multistep.StepAction {
	ctx := state.Get("ctx").(gocontext.Context)
	buildJob := state.Get("buildJob").(Job)
	instance := state.Get("instance").(backend.Instance)

	logWriter, err := buildJob.LogWriter(ctx)
	if err != nil {
		context.LoggerFromContext(ctx).WithField("err", err).Error("couldn't open a log writer")
		err := buildJob.Requeue()
		if err != nil {
			context.LoggerFromContext(ctx).WithField("err", err).Error("couldn't requeue job")
		}
		return multistep.ActionHalt
	}

	logWriter.SetTimeout(s.logTimeout)
	logWriter.SetMaxLogLength(s.maxLogLength)

	if hostname, ok := state.Get("hostname").(string); ok && hostname != "" {
		_, _ = logWriter.Write([]byte(fmt.Sprintf("Using worker: %s (%s)\n\n", hostname, instance.ID())))
	}

	state.Put("logWriter", logWriter)

	return multistep.ActionContinue
}

func (s *stepOpenLogWriter) Cleanup(state multistep.StateBag) {
	logWriter, ok := state.Get("logWriter").(LogWriter)
	if ok {
		logWriter.Close()
	}
}
