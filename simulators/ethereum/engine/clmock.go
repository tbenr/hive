package main

import (
	"math/big"
	"math/rand"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/catalyst"
	"github.com/ethereum/hive/hivesim"
)

// Consensus Layer Client Mock used to sync the Execution Clients once the TTD has been reached
type CLMocker struct {
	*hivesim.T
	// List of Engine Clients being served by the CL Mocker
	EngineClients []*EngineClient
	// Lock required so no client is offboarded during block production.
	EngineClientsLock sync.Mutex

	// Block Production Information
	NextBlockProducer       *EngineClient
	BlockProductionMustStop bool
	NextFeeRecipient        common.Address

	// PoS Chain History Information
	RandomHistory          map[uint64]common.Hash
	ExecutedPayloadHistory map[uint64]catalyst.ExecutableDataV1

	// Latest broadcasted data using the PoS Engine API
	LatestFinalizedNumber *big.Int
	LatestFinalizedHeader *types.Header
	LatestPayloadBuilt    catalyst.ExecutableDataV1
	LatestExecutedPayload catalyst.ExecutableDataV1
	LatestForkchoice      catalyst.ForkchoiceStateV1

	// Merge related
	FirstPoSBlockNumber         *big.Int
	TTDReached                  bool
	PoSBlockProductionActivated bool

	/* Set-Reset-Lock: Use LockSet() to guarantee that the test case finds the
	environment as expected, and not as previously modified by another test.

	The test cases can request a lock to "wake up" at a specific point during
	the PoS block production procedure and, when the CL mocker has reached it,
	the lock is released to let only one test case do its testing.
	*/
	PayloadBuildMutex                sync.Mutex
	NewGetPayloadMutex               sync.Mutex
	NewExecutePayloadMutex           sync.Mutex
	NewHeadBlockForkchoiceMutex      sync.Mutex
	NewSafeBlockForkchoiceMutex      sync.Mutex
	NewFinalizedBlockForkchoiceMutex sync.Mutex
}

func NewCLMocker(t *hivesim.T) *CLMocker {
	// Init random seed for different purposes
	seed := time.Now().Unix()
	t.Logf("Randomness seed: %v\n", seed)
	rand.Seed(seed)

	// Create the new CL mocker
	newCLMocker := &CLMocker{
		T:                           t,
		EngineClients:               make([]*EngineClient, 0),
		RandomHistory:               map[uint64]common.Hash{},
		ExecutedPayloadHistory:      map[uint64]catalyst.ExecutableDataV1{},
		LatestFinalizedHeader:       nil,
		PoSBlockProductionActivated: false,
		BlockProductionMustStop:     false,
		FirstPoSBlockNumber:         nil,
		LatestFinalizedNumber:       nil,
		TTDReached:                  false,
		NextFeeRecipient:            common.Address{},
		LatestForkchoice: catalyst.ForkchoiceStateV1{
			HeadBlockHash:      common.Hash{},
			SafeBlockHash:      common.Hash{},
			FinalizedBlockHash: common.Hash{},
		},
	}

	// Lock the mutexes. To be unlocked on next PoS block production.
	newCLMocker.PayloadBuildMutex.Lock()
	newCLMocker.NewGetPayloadMutex.Lock()
	newCLMocker.NewExecutePayloadMutex.Lock()
	newCLMocker.NewHeadBlockForkchoiceMutex.Lock()
	newCLMocker.NewSafeBlockForkchoiceMutex.Lock()
	newCLMocker.NewFinalizedBlockForkchoiceMutex.Lock()

	// Start timer to check when the TTD has been reached
	time.AfterFunc(tTDCheckPeriod, newCLMocker.checkTTD)

	return newCLMocker
}

// Add a Client to be kept in sync with the latest payloads
func (cl *CLMocker) AddEngineClient(newEngineClient *EngineClient) {
	cl.EngineClientsLock.Lock()
	defer cl.EngineClientsLock.Unlock()
	cl.EngineClients = append(cl.EngineClients, newEngineClient)
}

// Remove a Client to stop sending latest payloads
func (cl *CLMocker) RemoveEngineClient(removeEngineClient *EngineClient) {
	cl.EngineClientsLock.Lock()
	defer cl.EngineClientsLock.Unlock()
	i := -1
	for j := 0; j < len(cl.EngineClients); j++ {
		if cl.EngineClients[j] == removeEngineClient {
			i = j
			break
		}
	}
	if i >= 0 {
		cl.EngineClients[i] = cl.EngineClients[len(cl.EngineClients)-1]
		cl.EngineClients = cl.EngineClients[:len(cl.EngineClients)-1]
	}
}

// Helper struct to fetch the TotalDifficulty
type TD struct {
	TotalDifficulty *hexutil.Big `json:"totalDifficulty"`
}

// Check whether we have reached TTD and then enable PoS block production.
// This function must NOT be executed after we have reached TTD.
func (cl *CLMocker) checkTTD() {
	if len(cl.EngineClients) == 0 {
		// We have no clients running yet, we have not reached TTD
		time.AfterFunc(tTDCheckPeriod, cl.checkTTD)
		return
	}

	// Pick a random client to get the total difficulty of its head
	ec := cl.EngineClients[rand.Intn(len(cl.EngineClients))]

	var td *TD
	err := ec.c.CallContext(ec.Ctx(), &td, "eth_getBlockByNumber", "latest", false)
	if err != nil {
		cl.Fatalf("CLMocker: Could not get latest totalDifficulty: %v", err)
	}
	if td.TotalDifficulty.ToInt().Cmp(terminalTotalDifficulty) >= 0 {
		cl.TTDReached = true
		cl.LatestFinalizedHeader, err = ec.Eth.HeaderByNumber(ec.Ctx(), nil)
		if err != nil {
			cl.Fatalf("CLMocker: Could not get block header: %v", err)
		}
		cl.Logf("CLMocker: TTD has been reached at block %v\n", cl.LatestFinalizedHeader.Number)
		// Broadcast initial ForkchoiceUpdated
		cl.LatestForkchoice.HeadBlockHash = cl.LatestFinalizedHeader.Hash()
		cl.LatestForkchoice.SafeBlockHash = cl.LatestFinalizedHeader.Hash()
		cl.LatestForkchoice.FinalizedBlockHash = cl.LatestFinalizedHeader.Hash()
		for _, resp := range cl.broadcastForkchoiceUpdated(&cl.LatestForkchoice, nil) {
			if resp.Error != nil {
				cl.Logf("CLMocker: forkchoiceUpdated Error: %v\n", resp.Error)
			} else if resp.ForkchoiceResponse.Status != "SUCCESS" {
				cl.Logf("CLMocker: forkchoiceUpdated Response: %v\n", resp.ForkchoiceResponse)
			}
		}
		time.AfterFunc(PoSBlockProductionPeriod, cl.minePOSBlock)
		return
	}
	time.AfterFunc(tTDCheckPeriod, cl.checkTTD)
}

// Engine Block Production Methods
func (cl *CLMocker) stopPoSBlockProduction() {
	cl.BlockProductionMustStop = true
}

// Check whether a block number is a PoS block
func (cl *CLMocker) isBlockPoS(bn *big.Int) bool {
	if cl.FirstPoSBlockNumber == nil || cl.FirstPoSBlockNumber.Cmp(bn) > 0 {
		return false
	}
	return true
}

// Sets the fee recipient for the next block and returns the number where it will be included.
// A transaction can be included to be sent before getPayload if necessary
func (cl *CLMocker) setNextFeeRecipient(feeRecipient common.Address, ec *EngineClient, tx *types.Transaction) (*big.Int, error) {
	for {
		cl.PayloadBuildMutex.Lock()
		if ec == nil || (cl.NextBlockProducer != nil && cl.NextBlockProducer.Equals(ec)) {
			defer cl.PayloadBuildMutex.Unlock()
			cl.NextFeeRecipient = feeRecipient
			if tx != nil {
				err := ec.Eth.SendTransaction(ec.Ctx(), tx)
				if err != nil {
					return nil, err
				}
			}
			return big.NewInt(cl.LatestFinalizedNumber.Int64() + 1), nil
		}
		// Unlock and keep trying to get the requested Engine Client
		cl.PayloadBuildMutex.Unlock()
		time.Sleep(PoSBlockProductionPeriod)
	}
}

// Unlock all locks in the given CLMocker instance
func unlockAll(cl *CLMocker) {
	cl.PayloadBuildMutex.Unlock()
	cl.NewGetPayloadMutex.Unlock()
	cl.NewExecutePayloadMutex.Unlock()
	cl.NewHeadBlockForkchoiceMutex.Unlock()
	cl.NewSafeBlockForkchoiceMutex.Unlock()
	cl.NewFinalizedBlockForkchoiceMutex.Unlock()
}

// Mine a PoS block by using the Engine API
func (cl *CLMocker) minePOSBlock() {
	cl.EngineClientsLock.Lock()
	defer cl.EngineClientsLock.Unlock()
	if cl.BlockProductionMustStop {
		unlockAll(cl)
		return
	}
	var lastBlockNumber uint64
	var err error
	for {
		// Get a random client to generate the payload
		ec_id := rand.Intn(len(cl.EngineClients))
		cl.NextBlockProducer = cl.EngineClients[ec_id]

		lastBlockNumber, err = cl.NextBlockProducer.Eth.BlockNumber(cl.NextBlockProducer.Ctx())
		if err != nil {
			unlockAll(cl)
			cl.Fatalf("CLMocker: Could not get block number while selecting client for payload production (%v): %v", cl.NextBlockProducer.Client.Container, err)
		}

		lastBlockNumberBig := big.NewInt(int64(lastBlockNumber))

		if cl.LatestFinalizedNumber != nil && cl.LatestFinalizedNumber.Cmp(lastBlockNumberBig) != 0 {
			// Selected client is not synced to the last block number, try again
			continue
		}

		latestHeader, err := cl.NextBlockProducer.Eth.HeaderByNumber(cl.NextBlockProducer.Ctx(), lastBlockNumberBig)
		if err != nil {
			unlockAll(cl)
			cl.Fatalf("CLMocker: Could not get block header while selecting client for payload production (%v): %v", cl.NextBlockProducer.Client.Container, err)
		}

		lastBlockHash := latestHeader.Hash()

		if cl.LatestFinalizedHeader.Hash() != lastBlockHash {
			// Selected client latest block hash does not match canonical chain, try again
			continue
		} else {
			break
		}

	}

	cl.PayloadBuildMutex.Unlock()
	cl.PayloadBuildMutex.Lock()

	// Generate a random value for the Random field
	nextRandom := common.Hash{}
	rand.Read(nextRandom[:])

	payloadAttributes := catalyst.PayloadAttributesV1{
		Timestamp:             cl.LatestFinalizedHeader.Time + 1,
		Random:                nextRandom,
		SuggestedFeeRecipient: cl.NextFeeRecipient,
	}

	resp, err := cl.NextBlockProducer.EngineForkchoiceUpdatedV1(cl.NextBlockProducer.Ctx(), &cl.LatestForkchoice, &payloadAttributes)
	if err != nil {
		unlockAll(cl)
		cl.Fatalf("CLMocker: Could not send forkchoiceUpdatedV1 (%v): %v", cl.NextBlockProducer.Client.Container, err)
	}
	if resp.Status != "SUCCESS" {
		cl.Logf("CLMocker: forkchoiceUpdated Response: %v\n", resp)
	}

	cl.LatestPayloadBuilt, err = cl.NextBlockProducer.EngineGetPayloadV1(cl.NextBlockProducer.Ctx(), resp.PayloadID)
	if err != nil {
		unlockAll(cl)
		cl.Fatalf("CLMocker: Could not getPayload (%v, %v): %v", cl.NextBlockProducer.Client.Container, resp.PayloadID, err)
	}

	// Trigger actions for a new payload built.
	cl.NewGetPayloadMutex.Unlock()
	cl.NewGetPayloadMutex.Lock()

	// Broadcast the executePayload to all clients
	for i, resp := range cl.broadcastExecutePayload(&cl.LatestPayloadBuilt) {
		if resp.Error != nil {
			cl.Logf("CLMocker: broadcastExecutePayload Error (%v): %v\n", i, resp.Error)

		} else if resp.ExecutePayloadResponse.Status != "VALID" {
			cl.Logf("CLMocker: broadcastExecutePayload Response (%v): %v\n", i, resp.ExecutePayloadResponse)
		}
	}
	cl.LatestExecutedPayload = cl.LatestPayloadBuilt
	cl.ExecutedPayloadHistory[cl.LatestPayloadBuilt.Number] = cl.LatestPayloadBuilt

	// Trigger actions for new executePayload broadcast
	cl.NewExecutePayloadMutex.Unlock()
	cl.NewExecutePayloadMutex.Lock()

	// Broadcast forkchoice updated with new HeadBlock to all clients
	cl.LatestForkchoice.HeadBlockHash = cl.LatestPayloadBuilt.BlockHash
	for i, resp := range cl.broadcastForkchoiceUpdated(&cl.LatestForkchoice, nil) {
		if resp.Error != nil {
			cl.Logf("CLMocker: broadcastForkchoiceUpdated Error (%v): %v\n", i, resp.Error)
		} else if resp.ForkchoiceResponse.Status != "SUCCESS" {
			cl.Logf("CLMocker: broadcastForkchoiceUpdated Response (%v): %v\n", i, resp.ForkchoiceResponse)
		}
	}
	// Trigger actions for new HeadBlock forkchoice broadcast
	cl.NewHeadBlockForkchoiceMutex.Unlock()
	cl.NewHeadBlockForkchoiceMutex.Lock()

	// Broadcast forkchoice updated with new SafeBlock to all clients
	cl.LatestForkchoice.SafeBlockHash = cl.LatestPayloadBuilt.BlockHash
	for i, resp := range cl.broadcastForkchoiceUpdated(&cl.LatestForkchoice, nil) {
		if resp.Error != nil {
			cl.Logf("CLMocker: broadcastForkchoiceUpdated Error (%v): %v\n", i, resp.Error)
		} else if resp.ForkchoiceResponse.Status != "SUCCESS" {
			cl.Logf("CLMocker: broadcastForkchoiceUpdated Response (%v): %v\n", i, resp.ForkchoiceResponse)
		}
	}
	// Trigger actions for new SafeBlock forkchoice broadcast
	cl.NewSafeBlockForkchoiceMutex.Unlock()
	cl.NewSafeBlockForkchoiceMutex.Lock()

	// Broadcast forkchoice updated with new FinalizedBlock to all clients
	cl.LatestForkchoice.FinalizedBlockHash = cl.LatestPayloadBuilt.BlockHash
	for i, resp := range cl.broadcastForkchoiceUpdated(&cl.LatestForkchoice, nil) {
		if resp.Error != nil {
			cl.Logf("CLMocker: broadcastForkchoiceUpdated Error (%v): %v\n", i, resp.Error)
		} else if resp.ForkchoiceResponse.Status != "SUCCESS" {
			cl.Logf("CLMocker: broadcastForkchoiceUpdated Response (%v): %v\n", i, resp.ForkchoiceResponse)
		}
	}

	// Save random value
	cl.RandomHistory[cl.LatestFinalizedHeader.Number.Uint64()+1] = nextRandom

	// Save the number of the first PoS block
	if cl.FirstPoSBlockNumber == nil {
		cl.FirstPoSBlockNumber = big.NewInt(int64(cl.LatestFinalizedHeader.Number.Uint64() + 1))
	}

	// Save the header of the latest block in the PoS chain
	cl.LatestFinalizedNumber = big.NewInt(int64(lastBlockNumber + 1))

	// Check if any of the clients accepted the new payload
	cl.LatestFinalizedHeader = nil
	for _, ec := range cl.EngineClients {
		newHeader, err := ec.Eth.HeaderByNumber(cl.NextBlockProducer.Ctx(), cl.LatestFinalizedNumber)
		if err == nil {
			cl.LatestFinalizedHeader = newHeader
			break
		}
	}
	if cl.LatestFinalizedHeader == nil {
		unlockAll(cl)
		cl.Fatalf("CLMocker: None of the clients accepted the newly constructed payload")
	}

	// Switch protocol HTTP<>WS for all clients
	for _, ec := range cl.EngineClients {
		ec.SwitchProtocol()
	}

	// Trigger that we have finished producing a block
	cl.NewFinalizedBlockForkchoiceMutex.Unlock()

	// Exit if we need to
	if !cl.BlockProductionMustStop {
		// Lock BlockProducedMutex until we produce a new block
		cl.NewFinalizedBlockForkchoiceMutex.Lock()
		time.AfterFunc(PoSBlockProductionPeriod, cl.minePOSBlock)
	} else {
		unlockAll(cl)
	}
}

type ExecutePayloadOutcome struct {
	ExecutePayloadResponse *catalyst.ExecutePayloadResponse
	Error                  error
}

func (cl *CLMocker) broadcastExecutePayload(payload *catalyst.ExecutableDataV1) []ExecutePayloadOutcome {
	responses := make([]ExecutePayloadOutcome, len(cl.EngineClients))
	for i, ec := range cl.EngineClients {
		execPayloadResp, err := ec.EngineExecutePayloadV1(ec.Ctx(), payload)
		if err != nil {
			ec.Errorf("CLMocker: Could not ExecutePayloadV1: %v", err)
			responses[i].Error = err
		} else {
			cl.Logf("CLMocker: Executed payload: %v", execPayloadResp)
			responses[i].ExecutePayloadResponse = &execPayloadResp
		}
	}
	return responses
}

type ForkChoiceOutcome struct {
	ForkchoiceResponse *catalyst.ForkChoiceResponse
	Error              error
}

func (cl *CLMocker) broadcastForkchoiceUpdated(fcstate *catalyst.ForkchoiceStateV1, payloadAttr *catalyst.PayloadAttributesV1) []ForkChoiceOutcome {
	responses := make([]ForkChoiceOutcome, len(cl.EngineClients))
	for i, ec := range cl.EngineClients {
		fcUpdatedResp, err := ec.EngineForkchoiceUpdatedV1(ec.Ctx(), fcstate, payloadAttr)
		if err != nil {
			ec.Errorf("CLMocker: Could not ForkchoiceUpdatedV1: %v", err)
			responses[i].Error = err
		} else {
			responses[i].ForkchoiceResponse = &fcUpdatedResp
		}
	}
	return responses
}