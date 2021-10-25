/*
Copyright 2020 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package enricher

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/ReneKroon/ttlcache/v2"
	"github.com/go-logr/logr"
	"github.com/nxadm/tail"
	"github.com/pkg/errors"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"k8s.io/apimachinery/pkg/util/sets"
	rutil "sigs.k8s.io/release-utils/util"

	apienricher "sigs.k8s.io/security-profiles-operator/api/grpc/enricher"
	apimetrics "sigs.k8s.io/security-profiles-operator/api/grpc/metrics"
	"sigs.k8s.io/security-profiles-operator/internal/pkg/config"
	"sigs.k8s.io/security-profiles-operator/internal/pkg/util"
)

const (
	// defaultCacheTimeout is the timeout for the container ID and info cache being
	// used. The chosen value is nothing more than a rough guess.
	defaultCacheTimeout time.Duration = time.Hour

	defaultTimeout time.Duration = time.Minute
	maxMsgSize     int           = 16 * 1024 * 1024
)

// Enricher is the main structure of this package.
type Enricher struct {
	apienricher.UnimplementedEnricherServer
	impl             impl
	logger           logr.Logger
	containerIDCache ttlcache.SimpleCache
	infoCache        ttlcache.SimpleCache
	syscalls         sync.Map
	avcs             sync.Map
}

// New returns a new Enricher instance.
func New(logger logr.Logger) *Enricher {
	return &Enricher{
		impl:             &defaultImpl{},
		logger:           logger,
		containerIDCache: ttlcache.NewCache(),
		infoCache:        ttlcache.NewCache(),
		syscalls:         sync.Map{},
		avcs:             sync.Map{},
	}
}

// Run the log-enricher to scrap audit logs and enrich them with
// Kubernetes data (namespace, pod and container).
func (e *Enricher) Run() error {
	e.logger.Info(fmt.Sprintf("Setting up caches with expiry of %v", defaultCacheTimeout))
	for _, cache := range []ttlcache.SimpleCache{
		e.containerIDCache, e.infoCache,
	} {
		if err := e.impl.SetTTL(cache, defaultCacheTimeout); err != nil {
			return errors.Wrap(err, "set cache timeout")
		}
		defer cache.Close()
	}

	nodeName := e.impl.Getenv(config.NodeNameEnvKey)
	if nodeName == "" {
		err := errors.Errorf("%s environment variable not set", config.NodeNameEnvKey)
		e.logger.Error(err, "unable to run enricher")
		return err
	}

	e.logger.Info("Starting log-enricher on node: " + nodeName)

	e.logger.Info("Connecting to local GRPC server")
	var (
		conn          *grpc.ClientConn
		cancel        context.CancelFunc
		metricsClient apimetrics.Metrics_AuditIncClient
	)

	if err := util.Retry(func() (err error) {
		conn, cancel, err = e.impl.Dial()
		if err != nil {
			return errors.Wrap(err, "connecting to local GRPC server")
		}
		client := apimetrics.NewMetricsClient(conn)

		metricsClient, err = e.impl.AuditInc(client)
		if err != nil {
			cancel()
			e.impl.Close(conn)
			return errors.Wrap(err, "create metrics audit client")
		}

		return nil
	}, func(err error) bool { return true }); err != nil {
		return errors.Wrap(err, "connect to local GRPC server")
	}
	defer cancel()
	defer e.impl.Close(conn)

	if err := e.startGrpcServer(); err != nil {
		return errors.Wrap(err, "start GRPC server")
	}

	// Use auditd logs as main source or syslog as fallback.
	filePath := logFilePath()

	// If the file does not exist, then tail will wait for it to appear
	tailFile, err := e.impl.TailFile(
		filePath,
		tail.Config{
			ReOpen: true,
			Follow: true,
			Location: &tail.SeekInfo{
				Offset: 0,
				Whence: os.SEEK_END,
			},
		},
	)
	if err != nil {
		return errors.Wrap(err, "tailing file")
	}

	e.logger.Info("Reading from file " + filePath)
	for l := range e.impl.Lines(tailFile) {
		if l.Err != nil {
			e.logger.Error(l.Err, "failed to tail")
			continue
		}

		line := l.Text
		if !isAuditLine(line) {
			continue
		}

		auditLine, err := extractAuditLine(line)
		if err != nil {
			e.logger.Error(err, "extract seccomp details from audit line")
			continue
		}

		cID, err := e.getContainerID(auditLine.processID)
		if errors.Is(err, os.ErrNotExist) {
			// We're probably in container creation or removal
			continue
		}
		if err != nil {
			e.logger.Error(
				err, "unable to get container ID",
				"processID", auditLine.processID,
			)
			continue
		}

		info, err := e.getContainerInfo(nodeName, cID)
		if err != nil {
			e.logger.Error(
				err, "container ID not found in cluster",
				"processID", auditLine.processID,
				"containerID", cID,
			)
			continue
		}

		err = e.dispatchAuditLine(metricsClient, nodeName, auditLine, info)
		if err != nil {
			e.logger.Error(
				err, "dispatch audit line")
			continue
		}
	}

	return errors.Wrap(e.impl.Reason(tailFile), "enricher failed")
}

func (e *Enricher) startGrpcServer() error {
	e.logger.Info("Starting GRPC server API")

	listener, err := e.impl.Listen("tcp", addr())
	if err != nil {
		return errors.Wrap(err, "create listener")
	}

	grpcServer := grpc.NewServer(
		grpc.MaxSendMsgSize(maxMsgSize),
		grpc.MaxRecvMsgSize(maxMsgSize),
	)
	apienricher.RegisterEnricherServer(grpcServer, e)

	go func() {
		if err := e.impl.Serve(grpcServer, listener); err != nil {
			e.logger.Error(err, "unable to run GRPC server")
		}
	}()

	return nil
}

// Dial can be used to connect to the default GRPC server by creating a new
// client.
func Dial() (*grpc.ClientConn, context.CancelFunc, error) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	conn, err := grpc.DialContext(ctx, addr(), grpc.WithInsecure())
	if err != nil {
		cancel()
		return nil, nil, errors.Wrap(err, "GRPC dial")
	}
	return conn, cancel, nil
}

// addr returns the default server listening address.
func addr() string {
	return net.JoinHostPort("localhost", "9114")
}

func (e *Enricher) dispatchAuditLine(
	metricsClient apimetrics.Metrics_AuditIncClient,
	nodeName string,
	auditLine *auditLine,
	info *containerInfo,
) error {
	switch auditLine.type_ {
	case auditTypeSelinux:
		e.dispatchSelinuxLine(metricsClient, nodeName, auditLine, info)
	case auditTypeSeccomp:
		e.dispatchSeccompLine(metricsClient, nodeName, auditLine, info)
	default:
		return errors.Errorf("unknown audit line type %s", auditLine.type_)
	}

	return nil
}

func (e *Enricher) dispatchSelinuxLine(
	_ apimetrics.Metrics_AuditIncClient,
	nodeName string,
	auditLine *auditLine,
	info *containerInfo,
) {
	e.logger.Info("audit",
		"timestamp", auditLine.timestampID,
		"type", auditLine.type_,
		"profile", info.recordProfile,
		"node", nodeName,
		"namespace", info.namespace,
		"pod", info.podName,
		"container", info.containerName,
		"perm", auditLine.perm,
		"scontext", auditLine.scontext,
		"tcontext", auditLine.tcontext,
		"tclass", auditLine.tclass,
	)

	if info.recordProfile != "" {
		avc := &apienricher.AvcResponse_SelinuxAvc{
			Perm:     auditLine.perm,
			Scontext: auditLine.scontext,
			Tcontext: auditLine.tcontext,
			Tclass:   auditLine.tclass,
		}
		jsonBytes, err := protojson.Marshal(avc)
		if err != nil {
			e.logger.Error(err, "marshall protobuf")
		}

		a, _ := e.avcs.LoadOrStore(info.recordProfile, sets.NewString())
		a.(sets.String).Insert(string(jsonBytes))
	}
}

func (e *Enricher) dispatchSeccompLine(
	metricsClient apimetrics.Metrics_AuditIncClient,
	nodeName string,
	auditLine *auditLine,
	info *containerInfo,
) {
	syscallName, err := syscallName(auditLine.systemCallID)
	if err != nil {
		e.logger.Info(
			"no syscall name found for ID",
			"syscallID", auditLine.systemCallID,
			"err", err.Error(),
		)
		return
	}

	e.logger.Info("audit",
		"timestamp", auditLine.timestampID,
		"type", auditLine.type_,
		"node", nodeName,
		"namespace", info.namespace,
		"pod", info.podName,
		"container", info.containerName,
		"executable", auditLine.executable,
		"pid", auditLine.processID,
		"syscallID", auditLine.systemCallID,
		"syscallName", syscallName,
	)

	metricsType := apimetrics.AuditRequest_SECCOMP
	if err := e.impl.SendMetric(
		metricsClient,
		&apimetrics.AuditRequest{
			Type:       metricsType,
			Node:       nodeName,
			Namespace:  info.namespace,
			Pod:        info.podName,
			Container:  info.containerName,
			Executable: auditLine.executable,
			Syscall:    syscallName,
		},
	); err != nil {
		e.logger.Error(err, "unable to update metrics")
	}

	if info.recordProfile != "" {
		s, _ := e.syscalls.LoadOrStore(info.recordProfile, sets.NewString())
		s.(sets.String).Insert(syscallName)
	}
}

// logFilePath returns either the path to the audit logs or falls back to
// syslog if the audit log path does not exist.
func logFilePath() string {
	filePath := config.SyslogLogPath
	if rutil.Exists(config.AuditLogPath) {
		filePath = config.AuditLogPath
	}
	return filePath
}
