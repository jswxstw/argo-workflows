package sqldb

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	"github.com/upper/db/v4"

	wfv1 "github.com/argoproj/argo-workflows/v3/pkg/apis/workflow/v1alpha1"
	"github.com/argoproj/argo-workflows/v3/util/env"
	"github.com/argoproj/argo-workflows/v3/util/logging"
)

const OffloadNodeStatusDisabled = "Workflow has offloaded nodes, but offloading has been disabled"

type UUIDVersion struct {
	UID     string `db:"uid"`
	Version string `db:"version"`
}

type OffloadNodeStatusRepo interface {
	Save(ctx context.Context, uid, namespace string, nodes wfv1.Nodes) (string, error)
	Get(ctx context.Context, uid, version string) (wfv1.Nodes, error)
	List(ctx context.Context, namespace string) (map[UUIDVersion]wfv1.Nodes, error)
	ListOldOffloads(ctx context.Context, namespace string) (map[string][]string, error)
	Delete(ctx context.Context, uid, version string) error
	IsEnabled() bool
}

func NewOffloadNodeStatusRepo(ctx context.Context, log logging.Logger, session db.Session, clusterName, tableName string) (OffloadNodeStatusRepo, error) {
	// this environment variable allows you to make Argo Workflows delete offloaded data more or less aggressively,
	// useful for testing
	ttl := env.LookupEnvDurationOr(ctx, "OFFLOAD_NODE_STATUS_TTL", 5*time.Minute)
	log = log.WithField("ttl", ttl)
	log.Debug(ctx, "Node status offloading config")
	return &nodeOffloadRepo{session: session, clusterName: clusterName, tableName: tableName, ttl: ttl, log: log}, nil
}

type nodesRecord struct {
	ClusterName string `db:"clustername"`
	UUIDVersion
	Namespace string `db:"namespace"`
	Nodes     string `db:"nodes"`
}

type nodeOffloadRepo struct {
	session     db.Session
	clusterName string
	tableName   string
	// time to live - at what ttl an offload becomes old
	ttl time.Duration
	log logging.Logger
}

func (wdc *nodeOffloadRepo) IsEnabled() bool {
	return true
}

func nodeStatusVersion(s wfv1.Nodes) (string, string, error) {
	marshalled, err := json.Marshal(s)
	if err != nil {
		return "", "", err
	}

	h := fnv.New32()
	_, _ = h.Write(marshalled)
	return string(marshalled), fmt.Sprintf("fnv:%v", h.Sum32()), nil
}

func (wdc *nodeOffloadRepo) Save(ctx context.Context, uid, namespace string, nodes wfv1.Nodes) (string, error) {
	marshalled, version, err := nodeStatusVersion(nodes)
	if err != nil {
		return "", err
	}

	record := &nodesRecord{
		ClusterName: wdc.clusterName,
		UUIDVersion: UUIDVersion{
			UID:     uid,
			Version: version,
		},
		Namespace: namespace,
		Nodes:     marshalled,
	}

	logCtx := wdc.log.WithFields(logging.Fields{"uid": uid, "version": version})
	logCtx.Debug(ctx, "Offloading nodes")
	_, err = wdc.session.Collection(wdc.tableName).Insert(record)
	if err != nil {
		// if we have a duplicate, then it must have the same clustername+uid+version, which MUST mean that we
		// have already written this record
		if !isDuplicateKeyError(err) {
			return "", err
		}
		logCtx.WithField("err", err).Debug(ctx, "Ignoring duplicate key error")
	}
	// Don't need to clean up the old records here, we have a scheduled cleanup mechanism.
	// If we clean them up here, when we update, if there is an update conflict, we will not be able to go back.
	return version, nil
}

func isDuplicateKeyError(err error) bool {
	// postgres
	if strings.Contains(err.Error(), "duplicate key") {
		return true
	}
	// mysql
	if strings.Contains(err.Error(), "Duplicate entry") {
		return true
	}
	return false
}

func (wdc *nodeOffloadRepo) Get(ctx context.Context, uid, version string) (wfv1.Nodes, error) {
	wdc.log.WithFields(logging.Fields{"uid": uid, "version": version}).Debug(ctx, "Getting offloaded nodes")
	r := &nodesRecord{}
	err := wdc.session.SQL().
		SelectFrom(wdc.tableName).
		Where(db.Cond{"clustername": wdc.clusterName}).
		And(db.Cond{"uid": uid}).
		And(db.Cond{"version": version}).
		One(r)
	if err != nil {
		return nil, err
	}
	nodes := &wfv1.Nodes{}
	err = json.Unmarshal([]byte(r.Nodes), nodes)
	if err != nil {
		return nil, err
	}
	return *nodes, nil
}

func (wdc *nodeOffloadRepo) List(ctx context.Context, namespace string) (map[UUIDVersion]wfv1.Nodes, error) {
	wdc.log.WithFields(logging.Fields{"namespace": namespace}).Debug(ctx, "Listing offloaded nodes")
	var records []nodesRecord
	err := wdc.session.SQL().
		Select("uid", "version", "nodes").
		From(wdc.tableName).
		Where(db.Cond{"clustername": wdc.clusterName}).
		And(namespaceEqual(namespace)).
		All(&records)
	if err != nil {
		return nil, err
	}

	res := make(map[UUIDVersion]wfv1.Nodes)
	for _, r := range records {
		nodes := &wfv1.Nodes{}
		err = json.Unmarshal([]byte(r.Nodes), nodes)
		if err != nil {
			return nil, err
		}
		res[UUIDVersion{UID: r.UID, Version: r.Version}] = *nodes
	}

	return res, nil
}

func (wdc *nodeOffloadRepo) ListOldOffloads(ctx context.Context, namespace string) (map[string][]string, error) {
	wdc.log.WithFields(logging.Fields{"namespace": namespace}).Debug(ctx, "Listing old offloaded nodes")
	var records []UUIDVersion
	err := wdc.session.SQL().
		Select("uid", "version").
		From(wdc.tableName).
		Where(db.Cond{"clustername": wdc.clusterName}).
		And(namespaceEqual(namespace)).
		And(wdc.oldOffload()).
		All(&records)
	if err != nil {
		return nil, err
	}
	x := make(map[string][]string)
	for _, r := range records {
		x[r.UID] = append(x[r.UID], r.Version)
	}
	return x, nil
}

func (wdc *nodeOffloadRepo) Delete(ctx context.Context, uid, version string) error {
	if uid == "" {
		return fmt.Errorf("invalid uid")
	}
	if version == "" {
		return fmt.Errorf("invalid version")
	}
	logCtx := wdc.log.WithFields(logging.Fields{"uid": uid, "version": version})
	logCtx.Debug(ctx, "Deleting offloaded nodes")
	rs, err := wdc.session.SQL().
		DeleteFrom(wdc.tableName).
		Where(db.Cond{"clustername": wdc.clusterName}).
		And(db.Cond{"uid": uid}).
		And(db.Cond{"version": version}).
		Exec()
	if err != nil {
		return err
	}
	rowsAffected, err := rs.RowsAffected()
	if err != nil {
		return err
	}
	logCtx.WithField("rowsAffected", rowsAffected).Debug(ctx, "Deleted offloaded nodes")
	return nil
}

func (wdc *nodeOffloadRepo) oldOffload() string {
	return fmt.Sprintf("updatedat < current_timestamp - interval '%d' second", int(wdc.ttl.Seconds()))
}
