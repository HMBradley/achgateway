// Licensed to The Moov Authors under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. The Moov Authors licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package rdfi

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/moov-io/achgateway/internal/alerting"
	"github.com/moov-io/achgateway/internal/consul"
	"github.com/moov-io/achgateway/internal/service"
	"github.com/moov-io/achgateway/internal/upload"
	"github.com/moov-io/base/admin"
	"github.com/moov-io/base/log"
)

type Scheduler interface {
	Start() error
	Shutdown()
	RegisterRoutes(svc *admin.Server)
}

type PeriodicScheduler struct {
	logger       log.Logger
	odfi         *service.ODFIFiles
	sharding     service.Sharding
	uploadAgents service.UploadAgents

	ticker         *time.Ticker
	inboundTrigger chan manuallyTriggeredInbound
	shutdown       context.Context
	shutdownFunc   context.CancelFunc

	consul     *consul.Client
	downloader Downloader
	processors Processors

	alerters alerting.Alerters
}

func NewPeriodicScheduler(logger log.Logger, cfg *service.Config, consul *consul.Client, processors Processors) (Scheduler, error) {
	if cfg.Inbound.ODFI == nil {
		return nil, errors.New("missing Inbound ODFI config")
	}

	dl, err := NewDownloader(logger, cfg.Inbound.ODFI.Storage)
	if err != nil {
		return nil, err
	}

	alerters, err := alerting.NewAlerters(cfg.Errors)
	if err != nil {
		return nil, fmt.Errorf("ERROR creating alerters: %v", err)
	}

	ctx, cancelFunc := context.WithCancel(context.Background())

	return &PeriodicScheduler{
		logger:         logger,
		odfi:           cfg.Inbound.ODFI,
		sharding:       cfg.Sharding,
		uploadAgents:   cfg.Upload,
		ticker:         time.NewTicker(cfg.Inbound.ODFI.Interval),
		inboundTrigger: make(chan manuallyTriggeredInbound, 1),
		consul:         consul,
		downloader:     dl,
		processors:     processors,
		shutdown:       ctx,
		shutdownFunc:   cancelFunc,
		alerters:       alerters,
	}, nil
}

func (s *PeriodicScheduler) Shutdown() {
	if s == nil {
		return
	}
	s.logger.Info().Log("rdfi: shutting down periodic scheduler")
	s.shutdownFunc()
}

func (s *PeriodicScheduler) Start() error {
	for {
		select {
		case <-s.ticker.C:
			// Process each Organization we have an upload agent for
			s.tickAll()

		case waiter := <-s.inboundTrigger:
			// Process each Organization we have an upload agent for
			waiter.C <- s.tickAll()

		case <-s.shutdown.Done():
			s.logger.Log("scheduler shutdown")
			return nil
		}
	}
}

func (s *PeriodicScheduler) tickAll() error {
	for _, shardName := range s.odfi.ShardNames {
		shard := s.sharding.Find(shardName)
		if shard == nil {
			s.logger.Error().Logf("unable to find shard=%s", shardName)
			continue
		}

		logger := s.logger.With(log.Fields{
			"shard": log.String(shardName),
		})

		// Attempt to acquire leadership prior to processing
		leaderKey := fmt.Sprintf("achgateway/rdfi/%s", shardName)
		s.logger.Logf("attempting to acquire ODFI leadership for %s", leaderKey)

		// Acquire leadership for this shard
		err := consul.AcquireLock(logger, s.consul, leaderKey)
		if err != nil {
			logger.Info().Logf("skipping ODFI processing: %v", err)
		} else {
			s.logger.Info().Logf("starting rdfi periodic processing for %s", shard.Name)
			err := s.tick(shard)
			if err != nil {
				// Push this alert outside achgateway
				s.alertOnError(err)
				s.logger.Warn().Logf("error with rdfi periodic processing: %v", err)
			} else {
				s.logger.Info().Logf("finished rdfi periodic processing for %s", shard.Name)
			}
		}
	}
	return nil
}

func (s *PeriodicScheduler) tick(shard *service.Shard) error {
	agent, err := upload.New(s.logger, s.uploadAgents, shard.UploadAgent)
	if err != nil {
		return fmt.Errorf("agent: %v", err)
	}
	s.logger.Logf("start retrieving and processing of inbound files in %s", agent.Hostname())

	// Download and process files
	dl, err := s.downloader.CopyFilesFromRemote(agent)
	if err != nil {
		return fmt.Errorf("ERROR: problem copying files: %v", err)
	}

	// Setup presistor files into our configured audit trail
	auditSaver, err := newAuditSaver(agent.Hostname(), s.odfi.Audit)
	if err != nil {
		return fmt.Errorf("ERROR: %v", err)
	}

	// Run each processor over the files
	if err := ProcessFiles(dl, auditSaver, s.processors); err != nil {
		return fmt.Errorf("ERROR: processing files: %v", err)
	}

	// Start our cleanup routines
	if !s.odfi.Storage.KeepRemoteFiles {
		if err := Cleanup(s.logger, agent, dl); err != nil {
			return fmt.Errorf("ERROR: deleting remote files: %v", err)
		}
	}
	if s.odfi.Storage.RemoveZeroByteFiles {
		if err := CleanupEmptyFiles(s.logger, agent, dl); err != nil {
			return fmt.Errorf("ERROR: deleting zero byte files: %v", err)
		}
	}
	if s.odfi.Storage.CleanupLocalDirectory {
		return dl.deleteFiles()
	}
	return dl.deleteEmptyDirs(agent)
}

func (s *PeriodicScheduler) alertOnError(err error) {
	if s == nil {
		return
	}
	if err == nil {
		return
	}

	if err := s.alerters.AlertError(err); err != nil {
		s.logger.LogErrorf("ERROR sending alert: %v", err)
	}
}
