package engine

import (
	"context"
	"errors"
	"fmt"
	"github.com/alitto/pond"
	"github.com/go-streamline/core/flow/persist"
	"github.com/go-streamline/core/track"
	"github.com/go-streamline/engine/configuration"
	"github.com/go-streamline/interfaces/definitions"
	"github.com/go-streamline/interfaces/utils"
	"github.com/google/uuid"
	"github.com/robfig/cron/v3"
	"github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

var ErrCouldNotCreateFlowManager = fmt.Errorf("could not create flow manager")
var ErrCouldNotCreateDirs = fmt.Errorf("failed to create work directories")
var ErrRecoveryFailed = fmt.Errorf("failed to recover, if you don't want to recover, please delete the WAL file or set IgnoreRecoveryErrors to true")
var ErrCouldNotDeepCopyConfig = fmt.Errorf("could not deep copy config")

type Engine struct {
	config                *configuration.Config
	ctx                   context.Context
	cancelFunc            context.CancelFunc
	sessionUpdatesChannel chan definitions.SessionUpdate
	writeAheadLogger      definitions.WriteAheadLogger
	workerPool            workerPool
	log                   *logrus.Logger
	processorFactory      definitions.ProcessorFactory
	flowManager           definitions.FlowManager
	branchTracker         definitions.BranchTracker // Added branch tracker
	activeFlows           map[uuid.UUID]*definitions.Flow
	enabledProcessors     map[uuid.UUID]definitions.Processor
	triggerProcessors     map[uuid.UUID]triggerProcessorInfo
	scheduler             scheduler
}

func New(config *configuration.Config, writeAheadLogger definitions.WriteAheadLogger, log *logrus.Logger, processorFactory definitions.ProcessorFactory, flowManager definitions.FlowManager) (*Engine, error) {
	err := utils.CreateDirsIfNotExist(config.Workdir)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCouldNotCreateDirs, err)
	}

	config, err = DeepCopier.DeepCopyConfig(config)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCouldNotDeepCopyConfig, err)
	}

	return &Engine{
		config:                config,
		sessionUpdatesChannel: make(chan definitions.SessionUpdate),
		writeAheadLogger:      writeAheadLogger,
		workerPool:            pond.New(config.MaxWorkers, config.MaxWorkers),
		log:                   log,
		processorFactory:      processorFactory,
		flowManager:           flowManager,
		branchTracker:         track.NewBranchTracker(),
		activeFlows:           make(map[uuid.UUID]*definitions.Flow),
		enabledProcessors:     make(map[uuid.UUID]definitions.Processor),
		triggerProcessors:     make(map[uuid.UUID]triggerProcessorInfo),
		scheduler:             cron.New(cron.WithSeconds()),
	}, nil
}

func NewWithDefaults(config *configuration.Config, writeAheadLogger definitions.WriteAheadLogger, log *logrus.Logger, db *gorm.DB, processorFactory definitions.ProcessorFactory) (*Engine, error) {
	flowManager, err := persist.NewDBFlowManager(db)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCouldNotCreateFlowManager, err)
	}
	return New(config, writeAheadLogger, log, processorFactory, flowManager)
}

func (e *Engine) Close() error {
	e.cancelFunc()
	e.scheduler.Stop()
	e.workerPool.StopAndWait()
	var errs []error
	for _, triggerProcessor := range e.triggerProcessors {
		err := triggerProcessor.Processor.Close()
		if err != nil {
			errs = append(errs, err)
		}
	}
	for _, processor := range e.enabledProcessors {
		err := processor.Close()
		if err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

func (e *Engine) SessionUpdates() <-chan definitions.SessionUpdate {
	return e.sessionUpdatesChannel
}

func (e *Engine) Run() error {
	e.ctx, e.cancelFunc = context.WithCancel(context.Background())
	err := e.recover()
	if err != nil {
		return ErrRecoveryFailed
	}
	e.scheduler.Start()
	go e.monitorFlows()
	return nil
}
