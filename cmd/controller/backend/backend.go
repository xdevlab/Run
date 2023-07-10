/*
 *  Copyright (c) 2023 Juice Technologies, Inc. All Rights Reserved.
 */
package backend

import (
	"context"
	"errors"
	"time"

	"github.com/Juice-Labs/Juice-Labs/cmd/controller/storage"
	"github.com/Juice-Labs/Juice-Labs/pkg/gpu"
	"github.com/Juice-Labs/Juice-Labs/pkg/restapi"
	"github.com/Juice-Labs/Juice-Labs/pkg/task"
)

type Backend struct {
	storage storage.Storage
}

func NewBackend(storage storage.Storage) *Backend {
	return &Backend{
		storage: storage,
	}
}

func (backend *Backend) Run(group task.Group) error {
	err := backend.Update(group.Ctx())
	if err == nil {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-group.Ctx().Done():
				return err

			case <-ticker.C:
				err = backend.Update(group.Ctx())
				if err != nil {
					return err
				}
			}
		}
	}

	return err
}

func AgentMatches(agent restapi.Agent, requirements restapi.SessionRequirements) *gpu.SelectedGpuSet {
	// Need to ensure the agent has the GPU capacity to support this session
	gpuSet := gpu.NewGpuSet(agent.Gpus)

	// Add the currently assigned sessions to the gpuSet
	for _, session := range agent.Sessions {
		gpuSet.Select(session.Gpus)
	}

	// Determine if the gpuSet has the capacity
	selectedGpus, _ := gpuSet.Find(requirements.Gpus)
	return selectedGpus
}

func (backend *Backend) Update(ctx context.Context) error {
	err := backend.storage.SetAgentsMissingIfNotUpdatedFor(10 * time.Minute)
	if err != nil {
		return err
	}

	err = backend.storage.RemoveMissingAgentsIfNotUpdatedFor(time.Hour)
	if err != nil {
		return err
	}

	sessionIterator, err := backend.storage.GetQueuedSessionsIterator()
	if err != nil {
		return err
	}

	for sessionIterator.Next() {
		select {
		case <-ctx.Done():
			return nil

		default:
			session := sessionIterator.Value()

			// Get an iterator of the agents matching a subset of the requirements
			agentIterator, err_ := backend.storage.GetAvailableAgentsMatching(storage.TotalVramRequired(session.Requirements), session.Requirements.Tags, session.Requirements.Tolerates)
			err = errors.Join(err, err_)
			if err_ == nil {
				for agentIterator.Next() {
					agent := agentIterator.Value()

					selectedGpus := AgentMatches(agent, session.Requirements)
					if selectedGpus != nil {
						err = errors.Join(err, backend.storage.AssignSession(session.Id, agent.Id, selectedGpus.GetGpus()))
						break
					}
				}
			}
		}
	}

	return err
}
