package agentpool

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/forta-network/forta-node/clients"
	"github.com/forta-network/forta-node/clients/messaging"
	"github.com/forta-network/forta-node/config"
	"github.com/forta-network/forta-node/protocol"
	"github.com/forta-network/forta-node/services/scanner"
	"google.golang.org/grpc/codes"

	log "github.com/sirupsen/logrus"
)

// Constants
const (
	DefaultBufferSize = 100
)

// Agent receives blocks and transactions, and produces results.
type Agent struct {
	config config.AgentConfig

	evalTxCh     chan *protocol.EvaluateTxRequest
	txResults    chan<- *scanner.TxResult
	evalBlockCh  chan *protocol.EvaluateBlockRequest
	blockResults chan<- *scanner.BlockResult

	errCounter *errorCounter
	msgClient  clients.MessageClient

	client clients.AgentClient
	ready  bool
}

// NewAgent creates a new agent.
func NewAgent(agentCfg config.AgentConfig, msgClient clients.MessageClient, txResults chan<- *scanner.TxResult, blockResults chan<- *scanner.BlockResult) *Agent {
	return &Agent{
		config:       agentCfg,
		evalTxCh:     make(chan *protocol.EvaluateTxRequest, DefaultBufferSize),
		txResults:    txResults,
		evalBlockCh:  make(chan *protocol.EvaluateBlockRequest, DefaultBufferSize),
		blockResults: blockResults,
		errCounter:   NewErrorCounter(3, isCriticalErr),
		msgClient:    msgClient,
	}
}

func isCriticalErr(err error) bool {
	errStr := err.Error()
	return strings.Contains(errStr, codes.DeadlineExceeded.String()) ||
		strings.Contains(errStr, codes.Unavailable.String())
}

// Config returns the agent config.
func (agent *Agent) Config() config.AgentConfig {
	return agent.config
}

// Close implements io.Closer.
func (agent *Agent) Close() error {
	close(agent.evalTxCh)
	close(agent.evalBlockCh)
	agent.client.Close()
	return nil
}

func (agent *Agent) setClient(agentClient clients.AgentClient) {
	agent.client = agentClient
}

func (agent *Agent) processTransactions() {
	log := log.WithField("evaluate", "transaction").WithField("agent", agent.config.ID)
	for request := range agent.evalTxCh {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		log.Debugf("sending request")
		resp, err := agent.client.EvaluateTx(ctx, request)
		cancel()
		if err == nil {
			log.Debugf("request successful")
			resp.Metadata["imageHash"] = agent.config.ImageHash()
			agent.txResults <- &scanner.TxResult{
				AgentConfig: agent.config,
				Request:     request,
				Response:    resp,
			}
			continue
		}
		log.WithError(err).Error("error invoking agent")
		if agent.errCounter.TooManyErrs(err) {
			log.Error("too many errors - shutting down agent")
			agent.msgClient.Publish(messaging.SubjectAgentsActionStop, messaging.AgentPayload{agent.config})
			return
		}
	}
}

func (agent *Agent) processBlocks() {
	log := log.WithField("evaluate", "block").WithField("agent", agent.config.ID)
	for request := range agent.evalBlockCh {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		log.Debugf("sending request")
		resp, err := agent.client.EvaluateBlock(ctx, request)
		cancel()
		if err == nil {
			log.Debugf("request successful")
			resp.Metadata["imageHash"] = agent.config.ImageHash()
			agent.blockResults <- &scanner.BlockResult{
				AgentConfig: agent.config,
				Request:     request,
				Response:    resp,
			}
			continue
		}
		log.WithError(err).Error("error invoking agent")
		if agent.errCounter.TooManyErrs(err) {
			log.Error("too many errors - shutting down agent")
			agent.msgClient.Publish(messaging.SubjectAgentsActionStop, messaging.AgentPayload{agent.config})
			return
		}
	}
}

func (agent *Agent) shouldProcessBlock(blockNumber string) bool {
	n, _ := strconv.ParseUint(blockNumber, 10, 64)
	var isAtLeastStartBlock bool
	if agent.config.StartBlock != nil {
		isAtLeastStartBlock = *agent.config.StartBlock >= n
	} else {
		isAtLeastStartBlock = true
	}

	var isAtMostStopBlock bool
	if agent.config.StopBlock != nil {
		isAtMostStopBlock = *agent.config.StopBlock <= n
	} else {
		isAtMostStopBlock = true
	}

	return isAtLeastStartBlock && isAtMostStopBlock
}
