/*
 *  Copyright (c) 2023 Juice Technologies, Inc. All Rights Reserved.
 */
package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/lib/pq"
	_ "github.com/lib/pq"

	"github.com/Xdevlab/Run/cmd/controller/storage"
	"github.com/Xdevlab/Run/pkg/logger"
	"github.com/Xdevlab/Run/pkg/restapi"
)

type storageDriver struct {
	ctx context.Context
	db  *sql.DB
}

type sqlRow interface {
	Scan(dest ...any) error
}

type unmarshalFn[T any] func(row sqlRow) (T, error)

type tableIterator[T any] struct {
	ctx context.Context

	statement *sql.Stmt
	offset    int

	unmarshal unmarshalFn[T]

	iterator storage.Iterator[T]
}

func newIterator[T any](ctx context.Context, statement *sql.Stmt, unmarshal unmarshalFn[T]) (storage.Iterator[T], error) {
	iterator := &tableIterator[T]{
		ctx: ctx,

		statement: statement,
		offset:    0,

		unmarshal: unmarshal,
	}

	objects, err := iterator.retrieveRows()
	if err != nil {
		logger.Debugf("unable to retrieve rows, %s", err.Error())
		return nil, err
	}

	iterator.iterator = storage.NewDefaultIterator[T](objects)
	return iterator, err
}

func (iterator *tableIterator[T]) retrieveRows() ([]T, error) {
	rows, err := iterator.statement.QueryContext(iterator.ctx, iterator.offset)
	if err != nil {
		return nil, err
	}

	objects := make([]T, 0)
	for rows.Next() {
		obj, err := iterator.unmarshal(rows)
		if err != nil {
			return nil, err
		}

		objects = append(objects, obj)

		iterator.offset++
	}

	return objects, nil
}

func (iterator *tableIterator[T]) Next() bool {
	if !iterator.iterator.Next() {
		objects, err := iterator.retrieveRows()
		if err != nil {
			logger.Debugf("unable to retrieve rows, %s", err.Error())
			return false
		}

		if len(objects) == 0 {
			return false
		}

		iterator.iterator = storage.NewDefaultIterator[T](objects)
		// Call next once to move the new iterator to the first element
		iterator.iterator.Next()

	}

	return true
}

func (iterator *tableIterator[T]) Value() T {
	return iterator.iterator.Value()
}

const (
	selectAgents = `SELECT id, state, hostname, address, version, pool_id, gpus, 
			( SELECT ARRAY (
				SELECT ( SELECT row(key, value) FROM key_values WHERE id = agent_labels.key_value_id ) FROM agent_labels WHERE agent_id = agents.id
			) ) labels, 
			( SELECT ARRAY (
				SELECT ( SELECT row(key, value) FROM key_values WHERE id = agent_taints.key_value_id ) FROM agent_taints WHERE agent_id = agents.id
			) ) taints, 
			( SELECT ARRAY (
				SELECT row(id, state, address, version, pool_id, gpus) FROM sessions tab WHERE tab.agent_id = agents.id AND tab.state != 'closed'
			) ) sessions
		FROM agents`
	selectSessions       = "SELECT id, state, address, version, pool_id, gpus FROM sessions"
	selectQueuedSessions = "SELECT id, requirements FROM sessions WHERE state = 'queued'"

	orderBy     = " ORDER BY created_at ASC"
	offsetLimit = " OFFSET $1 LIMIT "
)

func selectAgentsWhere(where string) string {
	return fmt.Sprint(selectAgents, " WHERE ", where, orderBy)
}

func selectAgentsIterator(limit int) string {
	return fmt.Sprint(selectAgents, orderBy, offsetLimit, limit)
}

func selectAgentsIteratorWhere(where string, limit int) string {
	return fmt.Sprint(selectAgents, " WHERE ", where, orderBy, offsetLimit, limit)
}

func unmarshalAgent(row sqlRow) (restapi.Agent, error) {
	var gpus []byte
	var labels, taints, sessions pq.ByteaArray

	agent := restapi.Agent{
		Labels:   map[string]string{},
		Taints:   map[string]string{},
		Sessions: make([]restapi.Session, 0),
	}

	err := row.Scan(&agent.Id, &agent.State, &agent.Hostname, &agent.Address, &agent.Version, &agent.PoolId, &gpus, &labels, &taints, &sessions)
	if err != nil {
		if err == sql.ErrNoRows {
			err = storage.ErrNotFound
		}

		return restapi.Agent{}, err
	}

	err = json.Unmarshal(gpus, &agent.Gpus)
	if err != nil {
		return restapi.Agent{}, err
	}

	for _, label := range labels {
		var key, value string
		err = Composite(label).Scan(&key, &value)
		if err != nil {
			return restapi.Agent{}, err
		}

		agent.Labels[key] = value
	}

	for _, taint := range taints {
		var key, value string
		err = Composite(taint).Scan(&key, &value)
		if err != nil {
			return restapi.Agent{}, err
		}

		agent.Taints[key] = value
	}

	for _, sessionData := range sessions {
		session, err := unmarshalSession(Composite(sessionData))
		if err != nil {
			return restapi.Agent{}, err
		}

		agent.Sessions = append(agent.Sessions, session)
	}

	return agent, nil
}

func selectSessionsWhere(where string) string {
	return fmt.Sprint(selectSessions, " WHERE ", where, orderBy)
}

func unmarshalSession(row sqlRow) (restapi.Session, error) {
	var session restapi.Session
	var address []byte
	var gpus []byte

	var poolId sql.NullString

	err := row.Scan(&session.Id, &session.State, &address, &session.Version, &poolId, &gpus)

	if err != nil {
		if err == sql.ErrNoRows {
			err = storage.ErrNotFound
		}

		return restapi.Session{}, err
	}

	session.PoolId = poolId.String

	if address == nil {
		session.Address = ""
	} else {
		session.Address = string(address)
	}

	if gpus == nil {
		session.Gpus = nil
	} else {
		err = json.Unmarshal(gpus, &session.Gpus)
		if err != nil {
			return restapi.Session{}, err
		}
	}

	return session, nil
}

func selectQueuedSessionsWhere(where string) string {
	return fmt.Sprint(selectQueuedSessions, " AND ", where, orderBy)
}

func selectQueuedSessionsIteratorWhere(where string, limit int) string {
	return fmt.Sprint(selectQueuedSessions, " AND ", where, orderBy, offsetLimit, limit)
}

func unmarshalQueuedSession(row sqlRow) (storage.QueuedSession, error) {
	session := storage.QueuedSession{}

	var requirements string
	err := row.Scan(&session.Id, &requirements)
	if err != nil {
		return storage.QueuedSession{}, err
	}

	err = json.Unmarshal([]byte(requirements), &session.Requirements)
	if err != nil {
		return storage.QueuedSession{}, err
	}

	return session, nil
}

func OpenStorage(ctx context.Context, connection string) (storage.Storage, error) {
	db, err := sql.Open("postgres", connection)
	if err != nil {
		return nil, err
	}

	return &storageDriver{
		ctx: ctx,
		db:  db,
	}, nil
}

func (driver *storageDriver) Close() error {
	return driver.db.Close()
}

func (driver *storageDriver) AggregateData() (storage.AggregatedData, error) {
	var agents int
	var agentsByStatusArray pq.ByteaArray
	var sessions int
	var sessionsByStatusArray pq.ByteaArray

	row := driver.db.QueryRowContext(driver.ctx, `SELECT 
		(SELECT COUNT(*) FROM agents),
		ARRAY(SELECT row(state, COUNT(*)) FROM agents GROUP BY state),
		(SELECT COUNT(*) FROM sessions),
		ARRAY(SELECT row(state, COUNT(*)) FROM sessions GROUP BY state)`)
	err := row.Scan(&agents, &agentsByStatusArray, &sessions, &sessionsByStatusArray)
	if err != nil {
		return storage.AggregatedData{}, err
	}

	data := storage.AggregatedData{
		Agents:                   agents,
		AgentsByStatus:           map[string]int{},
		Sessions:                 sessions,
		SessionsByStatus:         map[string]int{},
		GpusByGpuName:            map[string]int{},
		VramByGpuName:            map[string]uint64{},
		VramUsedByGpuName:        map[string]uint64{},
		VramGBAvailableByGpuName: map[string]storage.Percentile[int]{},
		UtilizationByGpuName:     map[string]float64{},
		PowerDrawByGpuName:       map[string]float64{},
	}

	for _, composite := range agentsByStatusArray {
		var state string
		var count int
		Composite(composite).Scan(&state, &count)

		data.AgentsByStatus[state] = count
	}

	for _, composite := range sessionsByStatusArray {
		var state string
		var count int
		Composite(composite).Scan(&state, &count)

		data.SessionsByStatus[state] = count
	}

	vramGBAvailable := map[int]int{}
	vramGBAvailableByGpuName := map[string]map[int]int{}

	var utilization uint64
	utilizationByGpuName := map[string]uint64{}

	var powerDraw uint64
	powerDrawByGpuName := map[string]uint64{}

	rows, err := driver.db.QueryContext(driver.ctx, "SELECT gpus FROM agents WHERE state = 'active'")
	if err != nil {
		return storage.AggregatedData{}, err
	}

	for rows.Next() {
		var gpusData []byte
		err := rows.Scan(&gpusData)
		if err != nil {
			return storage.AggregatedData{}, err
		}

		var gpus []restapi.Gpu
		err = json.Unmarshal(gpusData, &gpus)
		if err != nil {
			return storage.AggregatedData{}, err
		}

		data.Gpus += len(gpus)
		for _, gpu := range gpus {
			data.GpusByGpuName[gpu.Name]++
			data.Vram += gpu.Vram
			data.VramByGpuName[gpu.Name] += gpu.Vram
			data.VramUsed += gpu.Metrics.VramUsed
			data.VramUsedByGpuName[gpu.Name] += gpu.Metrics.VramUsed

			gb := int((gpu.Vram - gpu.Metrics.VramUsed) / (1024 * 1024 * 1024))
			vramGBAvailable[gb]++

			if _, ok := vramGBAvailableByGpuName[gpu.Name]; !ok {
				vramGBAvailableByGpuName[gpu.Name] = map[int]int{}
			}

			vramGBAvailableByGpuName[gpu.Name][gb]++

			utilization += uint64(gpu.Metrics.UtilizationGpu)
			utilizationByGpuName[gpu.Name] += uint64(gpu.Metrics.UtilizationGpu)
			powerDraw += uint64(gpu.Metrics.PowerDraw)
			powerDrawByGpuName[gpu.Name] += uint64(gpu.Metrics.PowerDraw)
		}
	}

	data.PowerDraw = float64(powerDraw) / 1000.0
	for key, value := range powerDrawByGpuName {
		data.PowerDrawByGpuName[key] = float64(value) / 1000.0
	}

	if data.Gpus > 0 {
		data.Utilization = float64(utilization) / float64(data.Gpus)
		for key, value := range utilizationByGpuName {
			data.UtilizationByGpuName[key] = float64(value) / float64(data.Gpus)
		}

		calculatePercentiles := func(counts map[int]int, total int) storage.Percentile[int] {
			if len(counts) > 0 {
				sortedKeys := []int{}
				for key := range counts {
					sortedKeys = append(sortedKeys, key)
				}
				sort.Ints(sortedKeys)

				indexP90 := int(float64(total) * 0.90)
				indexP75 := int(float64(total) * 0.75)
				indexP50 := int(float64(total) * 0.50)
				indexP25 := int(float64(total) * 0.25)
				indexP10 := int(float64(total) * 0.10)

				percentile := storage.Percentile[int]{
					P100: sortedKeys[len(sortedKeys)-1],
				}

				index := 0
				keysIndex := 0
				key := sortedKeys[keysIndex]
				for keysIndex < len(sortedKeys) && index < indexP10 {
					key = sortedKeys[keysIndex]
					index += counts[key]
					keysIndex++
				}
				percentile.P10 = key

				for keysIndex < len(sortedKeys) && index < indexP25 {
					key = sortedKeys[keysIndex]
					index += counts[key]
					keysIndex++
				}
				percentile.P25 = key

				for keysIndex < len(sortedKeys) && index < indexP50 {
					key = sortedKeys[keysIndex]
					index += counts[key]
					keysIndex++
				}
				percentile.P50 = key

				for keysIndex < len(sortedKeys) && index < indexP75 {
					key = sortedKeys[keysIndex]
					index += counts[key]
					keysIndex++
				}
				percentile.P75 = key

				for keysIndex < len(sortedKeys) && index < indexP90 {
					key = sortedKeys[keysIndex]
					index += counts[key]
					keysIndex++
				}
				percentile.P90 = key

				return percentile
			}

			return storage.Percentile[int]{}
		}

		data.VramGBAvailable = calculatePercentiles(vramGBAvailable, data.Gpus)
		for key, gbAvailable := range vramGBAvailableByGpuName {
			data.VramGBAvailableByGpuName[key] = calculatePercentiles(gbAvailable, data.GpusByGpuName[key])
		}
	}

	return data, nil
}

func (driver *storageDriver) RegisterAgent(agent restapi.Agent) (string, error) {
	gpus, err := json.Marshal(agent.Gpus)
	if err != nil {
		return "", err
	}

	tx, err := driver.db.BeginTx(driver.ctx, nil)
	if err != nil {
		return "", err
	}

	var id string
	err = tx.QueryRowContext(driver.ctx, "INSERT INTO agents ("+
		"state, hostname, address, version, pool_id, gpus, vram_available, updated_at"+
		") VALUES ("+
		"$1, $2, $3, $4, $5, $6, $7, now()"+
		") RETURNING id",
		agent.State, agent.Hostname, agent.Address, agent.Version, agent.PoolId,
		gpus, storage.TotalVram(agent.Gpus)).Scan(&id)
	if err != nil {
		return "", errors.Join(err, tx.Rollback())
	}

	for key, value := range agent.Labels {
		_, err = tx.ExecContext(driver.ctx, "INSERT INTO key_values ("+
			"key, value"+
			") VALUES ("+
			"$1, $2"+
			") ON CONFLICT DO NOTHING", key, value)
		if err != nil {
			return "", errors.Join(err, tx.Rollback())
		}

		_, err = tx.ExecContext(driver.ctx, "INSERT INTO agent_labels ("+
			"agent_id, key_value_id"+
			") VALUES ("+
			"$1, (SELECT id FROM key_values WHERE key = $2 AND value = $3)"+
			")", id, key, value)
		if err != nil {
			return "", errors.Join(err, tx.Rollback())
		}
	}

	for key, value := range agent.Taints {
		_, err = tx.ExecContext(driver.ctx, "INSERT INTO key_values ("+
			"key, value"+
			") VALUES ("+
			"$1, $2"+
			") ON CONFLICT DO NOTHING", key, value)
		if err != nil {
			return "", errors.Join(err, tx.Rollback())
		}

		_, err = tx.ExecContext(driver.ctx, "INSERT INTO agent_taints ("+
			"agent_id, key_value_id"+
			") VALUES ("+
			"$1, (SELECT id FROM key_values WHERE key = $2 AND value = $3)"+
			")", id, key, value)
		if err != nil {
			return "", errors.Join(err, tx.Rollback())
		}
	}

	return id, tx.Commit()
}

func (driver *storageDriver) GetAgentById(id string) (restapi.Agent, error) {
	return unmarshalAgent(driver.db.QueryRowContext(driver.ctx, selectAgentsWhere("id = $1"), id))
}

func (driver *storageDriver) UpdateAgent(update restapi.AgentUpdate) error {
	var gpusData []byte
	err := driver.db.QueryRowContext(driver.ctx, "SELECT gpus FROM agents WHERE id = $1", update.Id).Scan(&gpusData)
	if err != nil {
		return err
	}

	var gpus []restapi.Gpu
	err = json.Unmarshal(gpusData, &gpus)
	if err != nil {
		return err
	}

	for index, metrics := range update.Gpus {
		gpus[index].Metrics = metrics
	}

	gpusData, err = json.Marshal(gpus)
	if err != nil {
		return err
	}

	tx, err := driver.db.BeginTx(driver.ctx, nil)
	if err != nil {
		return errors.Join(err, tx.Rollback())
	}

	for id, sessionUpdate := range update.SessionsUpdate {

		_, err = tx.ExecContext(driver.ctx, "UPDATE sessions SET state = $1 WHERE id = $2", sessionUpdate.State, id)
		if err != nil {
			return errors.Join(err, tx.Rollback())
		}

		for _, connectionUpdate := range sessionUpdate.Connections {
			_, err = tx.ExecContext(driver.ctx, `
			INSERT INTO connections (id, session_id, pid, process_name, exit_code)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (id) 
			DO UPDATE SET exit_code = $5`,
				connectionUpdate.Id, id, connectionUpdate.Pid, connectionUpdate.ProcessName, connectionUpdate.ExitCode)
			if err != nil {
				return errors.Join(err, tx.Rollback())
			}
		}
	}

	if update.State != "" {
		_, err = tx.ExecContext(driver.ctx, "UPDATE agents SET state = $1, gpus = $2, updated_at = now() WHERE id = $3", update.State, gpusData, update.Id)
	} else {
		_, err = tx.ExecContext(driver.ctx, "UPDATE agents SET gpus = $1, updated_at = now() WHERE id = $2", gpusData, update.Id)
	}

	if err != nil {
		return errors.Join(err, tx.Rollback())
	}

	return tx.Commit()
}

func NewNullString(s string) sql.NullString {
	if len(s) == 0 {
		return sql.NullString{}
	}
	return sql.NullString{
		String: s,
		Valid:  true,
	}
}

func (driver *storageDriver) RequestSession(sessionRequirements restapi.SessionRequirements) (string, error) {
	requirements, err := json.Marshal(sessionRequirements)
	if err != nil {
		return "", err
	}

	tx, err := driver.db.BeginTx(driver.ctx, nil)
	if err != nil {
		return "", errors.Join(err, tx.Rollback())
	}

	var id string
	err = tx.QueryRowContext(driver.ctx, "INSERT INTO sessions ("+
		"state, version, pool_id, requirements, vram_required, updated_at"+
		") VALUES ("+
		"$1, $2, $3, $4, $5, $6, now()"+
		") RETURNING id",
		restapi.SessionQueued, sessionRequirements.Version, NewNullString(sessionRequirements.PoolId),
		requirements, storage.TotalVramRequired(sessionRequirements)).Scan(&id)
	if err != nil {
		return "", errors.Join(err, tx.Rollback())
	}

	for key, value := range sessionRequirements.MatchLabels {
		_, err = tx.ExecContext(driver.ctx, "INSERT INTO key_values ("+
			"key, value"+
			") VALUES ("+
			"$1, $2"+
			") ON CONFLICT DO NOTHING", key, value)
		if err != nil {
			return "", errors.Join(err, tx.Rollback())
		}

		_, err = tx.ExecContext(driver.ctx, "INSERT INTO session_match_labels ("+
			"session_id, key_value_id"+
			") VALUES ("+
			"$1, (SELECT id FROM key_values WHERE key = $2 AND value = $3)"+
			")", id, key, value)
		if err != nil {
			return "", errors.Join(err, tx.Rollback())
		}
	}

	for key, value := range sessionRequirements.Tolerates {
		_, err = tx.ExecContext(driver.ctx, "INSERT INTO key_values ("+
			"key, value"+
			") VALUES ("+
			"$1, $2"+
			") ON CONFLICT DO NOTHING", key, value)
		if err != nil {
			return "", errors.Join(err, tx.Rollback())
		}

		_, err = tx.ExecContext(driver.ctx, "INSERT INTO session_tolerates ("+
			"session_id, key_value_id"+
			") VALUES ("+
			"$1, (SELECT id FROM key_values WHERE key = $2 AND value = $3)"+
			")", id, key, value)
		if err != nil {
			return "", errors.Join(err, tx.Rollback())
		}
	}

	return id, tx.Commit()
}

func (driver *storageDriver) AssignSession(sessionId string, agentId string, gpus []restapi.SessionGpu) error {
	gpusData, err := json.Marshal(gpus)
	if err != nil {
		return err
	}

	tx, err := driver.db.BeginTx(driver.ctx, nil)
	if err != nil {
		return errors.Join(err, tx.Rollback())
	}

	_, err = tx.ExecContext(driver.ctx, `UPDATE agents SET vram_available = vram_available - (
			SELECT vram_required FROM sessions WHERE id = $1
		), updated_at = now() WHERE id = $2`, sessionId, agentId)
	if err != nil {
		return errors.Join(err, tx.Rollback())
	}

	_, err = tx.ExecContext(driver.ctx, `UPDATE sessions SET agent_id = $1, state = $2, address = (
			SELECT address FROM agents WHERE id = $1
		), gpus = $3, updated_at = now() WHERE id = $4`, agentId, restapi.SessionAssigned, gpusData, sessionId)
	if err != nil {
		return errors.Join(err, tx.Rollback())
	}

	return tx.Commit()
}

func (driver *storageDriver) CancelSession(sessionId string) error {
	_, err := driver.db.ExecContext(driver.ctx, `UPDATE sessions s SET
		state = CASE WHEN s.agent_id IS NULL
					THEN 'closed'::session_state
					ELSE 'canceling'::session_state
				END
		WHERE s.id = $1`, sessionId)
	return err
}

func (driver *storageDriver) GetSessionById(id string) (restapi.Session, error) {
	session, err := unmarshalSession(driver.db.QueryRowContext(driver.ctx, selectSessionsWhere("id = $1"), id))
	if err != nil {
		return restapi.Session{}, err
	}
	connectionRows, err := driver.db.QueryContext(driver.ctx, "SELECT id, pid, process_name, exit_code FROM connections WHERE session_id = $1", id)
	if err != nil {
		return restapi.Session{}, err
	}
	for connectionRows.Next() {
		var connection restapi.Connection
		err = connectionRows.Scan(&connection.Id, &connection.Pid, &connection.ProcessName, &connection.ExitCode)
		if err != nil {
			return restapi.Session{}, err
		}
		session.Connections = append(session.Connections, connection)
	}

	return session, nil

}

func (driver *storageDriver) GetQueuedSessionById(id string) (storage.QueuedSession, error) {
	return unmarshalQueuedSession(driver.db.QueryRowContext(driver.ctx, selectQueuedSessionsWhere("id = $1"), id))
}

func (driver *storageDriver) GetAgents(poolId string) (storage.Iterator[restapi.Agent], error) {
	var statement *sql.Stmt
	var err error

	if poolId != "" {
		statement, err = driver.db.PrepareContext(driver.ctx, selectAgentsIteratorWhere(fmt.Sprintf("pool_id = '%s' AND state = 'active'", poolId), 20))
	} else {
		statement, err = driver.db.PrepareContext(driver.ctx, selectAgentsIteratorWhere("state = 'active'", 20))
	}

	if err != nil {
		return nil, err
	}

	return newIterator(driver.ctx, statement, unmarshalAgent)
}

func (driver *storageDriver) GetAvailableAgentsMatching(totalAvailableVramAtLeast uint64) (storage.Iterator[restapi.Agent], error) {
	statement, err := driver.db.PrepareContext(driver.ctx, selectAgentsIteratorWhere(
		fmt.Sprint("state = 'active' AND vram_available >= ", totalAvailableVramAtLeast), 20))
	if err != nil {
		return nil, err
	}

	return newIterator(driver.ctx, statement, unmarshalAgent)
}

func (driver *storageDriver) GetQueuedSessionsIterator() (storage.Iterator[storage.QueuedSession], error) {
	statement, err := driver.db.PrepareContext(driver.ctx, selectQueuedSessionsIteratorWhere("state = 'queued'", 20))
	if err != nil {
		return nil, err
	}

	return newIterator(driver.ctx, statement, unmarshalQueuedSession)
}

func (driver *storageDriver) SetAgentsMissingIfNotUpdatedFor(duration time.Duration) error {
	_, err := driver.db.ExecContext(driver.ctx, "UPDATE agents SET state = 'missing', updated_at = now() WHERE state = 'active' AND updated_at <= now()-make_interval(secs=>$1)", duration.Seconds())
	return err
}

func (driver *storageDriver) RemoveMissingAgentsIfNotUpdatedFor(duration time.Duration) error {
	_, err := driver.db.ExecContext(driver.ctx, "DELETE FROM agents WHERE state = 'missing' AND updated_at <= now()-make_interval(secs=>$1)", duration.Seconds())
	return err
}

func (driver *storageDriver) DeletePool(id string) error {
	_, err := driver.db.ExecContext(driver.ctx, "DELETE FROM pools WHERE id = $1", id)
	return err
}

func (driver *storageDriver) GetPool(id string) (restapi.Pool, error) {
	row := driver.db.QueryRowContext(driver.ctx, "SELECT id, pool_name FROM pools WHERE id = $1", id)
	var pool restapi.Pool
	err := row.Scan(&pool.Id, &pool.Name)
	if err != nil {
		if err == sql.ErrNoRows {
			err = storage.ErrNotFound
		}
		return restapi.Pool{}, err
	}
	return pool, nil
}

func (driver *storageDriver) CreatePool(name string) (restapi.Pool, error) {
	tx, err := driver.db.BeginTx(driver.ctx, nil)
	if err != nil {
		return restapi.Pool{}, err
	}

	var pool restapi.Pool
	err = tx.QueryRowContext(driver.ctx, "INSERT INTO pools (pool_name) VALUES ($1) RETURNING id", name).Scan(&pool.Id)
	if err != nil {
		return restapi.Pool{}, errors.Join(err, tx.Rollback())
	}

	pool.Name = name
	err = tx.Commit()
	if err != nil {
		return restapi.Pool{}, err
	}
	return pool, nil
}

func (driver *storageDriver) AddPermission(poolId string, userId string, permission restapi.Permission) error {
	tx, err := driver.db.BeginTx(driver.ctx, nil)
	if err != nil {
		return err
	}

	_, err = tx.ExecContext(driver.ctx, "INSERT INTO permissions (user_id, pool_id, permission) VALUES ($1, $2, $3)", userId, poolId, permission)
	if err != nil {
		return errors.Join(err, tx.Rollback())
	}

	return tx.Commit()
}

func (driver *storageDriver) RemovePermission(poolId string, userId string, permission restapi.Permission) error {
	tx, err := driver.db.BeginTx(driver.ctx, nil)
	if err != nil {
		return err
	}

	result, err := tx.ExecContext(driver.ctx, "DELETE FROM permissions WHERE user_id = $1 AND pool_id = $2 AND permission = $3", userId, poolId, permission)
	if err != nil {
		return errors.Join(err, tx.Rollback())
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return errors.Join(err, tx.Rollback())
	}
	if rowsAffected == 0 {
		return errors.New("No permission found")
	}

	return tx.Commit()

}

type UserPermissionRow struct {
	PoolId       string
	Permission   restapi.Permission
	PoolName     string
	SessionCount int
	AgentCount   int
	UserCount    int
}

func (driver *storageDriver) GetPermissions(userId string) (restapi.UserPermissions, error) {
	var result restapi.UserPermissions

	rows, err := driver.db.QueryContext(driver.ctx, `
	SELECT permissions.pool_id, permissions.permission, pools.pool_name, COUNT(DISTINCT sessions.id) AS session_count, COUNT(DISTINCT agents.id) AS agent_count, 
		(SELECT COUNT(DISTINCT p.user_id) FROM permissions p WHERE p.pool_id = permissions.pool_id) as user_count
	FROM permissions 
	JOIN pools ON pools.id = permissions.pool_id
	LEFT JOIN agents ON agents.pool_id = pools.id AND agents.state = 'active'
	LEFT JOIN sessions ON sessions.agent_id = agents.id AND sessions.state = 'active'
	WHERE user_id = $1
	GROUP BY permissions.pool_id, permissions.permission, pools.pool_name`, userId)

	if err != nil {
		return restapi.UserPermissions{}, err
	}
	defer rows.Close()

	for rows.Next() {
		var row UserPermissionRow
		err := rows.Scan(&row.PoolId, &row.Permission, &row.PoolName, &row.SessionCount, &row.AgentCount, &row.UserCount)
		if err != nil {
			return restapi.UserPermissions{}, err
		}
		if result.Permissions == nil {
			result.Permissions = make(map[restapi.Permission][]restapi.Pool)
		}
		if result.Permissions[row.Permission] == nil {
			result.Permissions[row.Permission] = []restapi.Pool{}
		}
		result.Permissions[row.Permission] = append(result.Permissions[row.Permission], restapi.Pool{
			Id:           row.PoolId,
			Name:         row.PoolName,
			SessionCount: row.SessionCount,
			AgentCount:   row.AgentCount,
			UserCount:    row.UserCount,
		})
	}

	return result, nil

}

type PoolPermissionRow struct {
	PoolId     string
	Permission restapi.Permission
	UserId     string
}

func (driver *storageDriver) GetPoolPermissions(poolId string) (restapi.PoolPermissions, error) {
	var result restapi.PoolPermissions

	rows, err := driver.db.QueryContext(driver.ctx, `
	SELECT permissions.pool_id, permissions.permission, permissions.user_id
	FROM permissions 
	WHERE permissions.pool_id = $1`, poolId)

	if err != nil {
		return restapi.PoolPermissions{}, err
	}
	defer rows.Close()

	for rows.Next() {
		var row PoolPermissionRow
		err := rows.Scan(&row.PoolId, &row.Permission, &row.UserId)
		if err != nil {
			return restapi.PoolPermissions{}, err
		}
		if result.UserIds == nil {
			result.UserIds = make(map[string][]restapi.Permission)
		}
		if result.UserIds[row.UserId] == nil {
			result.UserIds[row.UserId] = []restapi.Permission{}
		}
		result.UserIds[row.UserId] = append(result.UserIds[row.UserId], row.Permission)
	}

	return result, nil

}
