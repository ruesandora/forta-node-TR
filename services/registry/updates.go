package registry

import (
	"OpenZeppelin/fortify-node/clients/messaging"
	"OpenZeppelin/fortify-node/config"
	"OpenZeppelin/fortify-node/contracts"
	"OpenZeppelin/fortify-node/domain"
	"OpenZeppelin/fortify-node/utils"
	"encoding/json"
	"fmt"
	"io"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	log "github.com/sirupsen/logrus"
)

func (rs *RegistryService) detectAgentEvents(evt *domain.TransactionEvent) (err error) {
	for _, logEntry := range evt.Receipt.Logs {
		log := transformLog(&logEntry)

		var addedEvent *contracts.AgentRegistryAgentAdded
		addedEvent, err = rs.logUnpacker.UnpackAgentRegistryAgentAdded(log)
		if err == nil {
			if (common.Hash)(addedEvent.PoolId).String() != rs.poolID.String() {
				continue
			}
			return rs.sendAgentUpdate(&agentUpdate{IsCreation: true}, addedEvent.AgentId, addedEvent.Ref)
		}

		var updatedEvent *contracts.AgentRegistryAgentUpdated
		updatedEvent, err = rs.logUnpacker.UnpackAgentRegistryAgentUpdated(log)
		if err == nil {
			if (common.Hash)(updatedEvent.PoolId).String() != rs.poolID.String() {
				continue
			}
			return rs.sendAgentUpdate(&agentUpdate{IsUpdate: true}, updatedEvent.AgentId, updatedEvent.Ref)
		}

		var removedEvent *contracts.AgentRegistryAgentRemoved
		removedEvent, err = rs.logUnpacker.UnpackAgentRegistryAgentRemoved(log)
		if err == nil {
			if (common.Hash)(removedEvent.PoolId).String() != rs.poolID.String() {
				continue
			}
			return rs.sendAgentUpdate(&agentUpdate{IsRemoval: true}, removedEvent.AgentId, "")
		}
	}
	return nil
}

type agentFile struct {
	Manifest struct {
		ImageReference string `json:"imageReference"`
	} `json:"manifest"`
}

func (rs *RegistryService) sendAgentUpdate(update *agentUpdate, agentID [32]byte, ref string) error {
	agentCfg, err := rs.makeAgentConfig(agentID, ref)
	if err != nil {
		return err
	}

	update.Config = agentCfg
	rs.agentUpdates <- update
	return nil
}

func (rs *RegistryService) makeAgentConfig(agentID [32]byte, ref string) (agentCfg config.AgentConfig, err error) {
	agentCfg.ID = (common.Hash)(agentID).String()
	if len(ref) == 0 {
		return
	}

	var (
		r io.ReadCloser
	)
	for i := 0; i < 10; i++ {
		r, err = rs.ipfsClient.Cat(fmt.Sprintf("/ipfs/%s", ref))
		if err == nil {
			break
		}
	}
	if err != nil {
		err = fmt.Errorf("failed to load the agent file using ipfs ref: %v", err)
		return
	}
	defer r.Close()

	var agentData agentFile
	if err = json.NewDecoder(r).Decode(&agentData); err != nil {
		err = fmt.Errorf("failed to decode the agent file: %v", err)
		return
	}

	var ok bool
	agentCfg.Image, ok = utils.ValidateImageRef(rs.cfg.Registry.ContainerRegistry, agentData.Manifest.ImageReference)
	if !ok {
		log.Warnf("invalid agent reference - skipping: %s", agentCfg.Image)
	}

	return
}

func transformLog(log *domain.LogEntry) *types.Log {
	transformed := &types.Log{
		Data: []byte(*log.Data),
	}
	for _, topic := range log.Topics {
		transformed.Topics = append(transformed.Topics, common.HexToHash(*topic))
	}
	return transformed
}

func (rs *RegistryService) listenToAgentUpdates() {
	for update := range rs.agentUpdates {
		rs.agentUpdatesWg.Wait()
		rs.handleAgentUpdate(update)
		rs.msgClient.Publish(messaging.SubjectAgentsVersionsLatest, rs.agentsConfigs)
	}
}

func (rs *RegistryService) handleAgentUpdate(update *agentUpdate) {
	switch {
	case update.IsCreation:
		// Skip if we already have this agent.
		for _, agent := range rs.agentsConfigs {
			if agent.ID == update.Config.ID {
				return
			}
		}
		rs.agentsConfigs = append(rs.agentsConfigs, update.Config)

	case update.IsUpdate:
		for _, agent := range rs.agentsConfigs {
			if agent.ID == update.Config.ID {
				agent.Image = update.Config.Image
				// TODO: Also update start and stop block when this data is available.
				return
			}
		}

	case update.IsRemoval:
		var newAgents []config.AgentConfig
		for _, agent := range rs.agentsConfigs {
			if agent.ID != update.Config.ID {
				newAgents = append(newAgents, agent)
			}
		}
		rs.agentsConfigs = newAgents

	default:
		log.Panicf("tried to handle unknown agent update")
	}
}
