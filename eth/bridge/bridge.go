package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/ethereum/go-ethereum/log"
)

// Add these variables at the package level
var (
	// New variables for block tracking
	processedBlocks      = make(map[uint64]*BlockInfo) // map[blockNumber]*BlockInfo
	processedBlocksMu    sync.RWMutex
	lastFinalizedBlock   uint64
	lastFinalizedBlockMu sync.RWMutex
)

// Configuration types
type BridgeEndpointConfig struct {
	URL     string
	Enabled bool
}

type BridgeConfig struct {
	Endpoints      []BridgeEndpointConfig
	RequestTimeout time.Duration
}

// BridgeTransferRequest represents a transfer request to the bridge service
type BridgeTransferRequest struct {
	Chain        string `json:"chain"`        // Source chain (always "ethereum" in this case)
	Contract     string `json:"contract"`     // Token contract address
	Owner        string `json:"owner"`        // Original token sender
	Amount       string `json:"amount"`       // Token amount as string
	ReceiptHash  string `json:"receiptHash"`  // Ethereum receipt hash
	ReceiptIndex string `json:"receiptIndex"` // Index of the receipt inside the block as string
	BlockNumber  string `json:"blockNumber"`  // Block number as string
	ReceiptsRoot string `json:"receiptsRoot"` // Receipts root hash of the block
}

// The USDT contract address on Ethereum mainnet
var USDTContractAddress = common.HexToAddress("0xdAC17F958D2ee523a2206206994597C13D831ec7")

// Known Binance addresses (add more as needed)
var FilteredAddresses = []common.Address{
	common.HexToAddress("0x9696f59E4d72E237BE84fFD425DCaD154Bf96976"), // Binance 18
}

// TransferSignature is the event signature for ERC20 Transfer events
var TransferSignature = crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))

// Add new types and variables for block tracking
type BlockInfo struct {
	Number       uint64
	Hash         common.Hash
	ReceiptsRoot common.Hash
	Finalized    bool
	Config       BridgeConfig
	HasTransfers bool // Indicates if this block has relevant transfers
}

// sendToBridge sends transaction details to all configured bridge endpoints
func sendToBridge(ctx context.Context, contract common.Address, from common.Address, amount *big.Int, blockNum uint64, receiptsRoot common.Hash, receiptIndex uint, config BridgeConfig) error {
	// Prepare request payload using the specified format
	payload := BridgeTransferRequest{
		Chain:        "ethereum",
		Contract:     contract.Hex(),
		Owner:        from.Hex(),
		Amount:       amount.String(),
		ReceiptIndex: fmt.Sprintf("%d", receiptIndex),
		BlockNumber:  fmt.Sprintf("%d", blockNum),
		ReceiptsRoot: receiptsRoot.Hex(),
	}

	// Marshal the payload to JSON
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal JSON payload: %w", err)
	}

	// Send to each enabled endpoint
	for i, endpoint := range config.Endpoints {
		if !endpoint.Enabled {
			continue
		}

		// Launch a goroutine for each endpoint
		go func(url string, index int) {
			err := sendToSingleEndpoint(ctx, url, jsonData, config.RequestTimeout)
			if err != nil {
				log.Error("Failed to send transfer to bridge endpoint",
					"url", url,
					"index", index,
					"err", err)
			} else {
				log.Info("Successfully sent transfer to bridge endpoint",
					"url", url,
					"index", index,
					"from", from.Hex(),
					"amount", amount.String(),
					"receiptshash", receiptsRoot.Hex())
			}
		}(endpoint.URL, i)
	}

	return nil
}

// sendToSingleEndpoint sends data to a single bridge endpoint
func sendToSingleEndpoint(ctx context.Context, url string, jsonData []byte, timeout time.Duration) error {
	// Create HTTP request with timeout
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("failed to create HTTP request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// Execute the request
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send HTTP request: %w", err)
	}
	defer resp.Body.Close()

	// Check response status
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("API returned non-200 status: %d", resp.StatusCode)
	}

	return nil
}

// Store block information for later finalization
func StoreBlockInfo(number uint64, hash, receiptsRoot common.Hash, config BridgeConfig) {
	processedBlocksMu.Lock()
	defer processedBlocksMu.Unlock()

	processedBlocks[number] = &BlockInfo{
		Number:       number,
		Hash:         hash,
		ReceiptsRoot: receiptsRoot,
		Finalized:    false,
		Config:       config,
		HasTransfers: false,
	}
	log.Debug("Stored block info for future finalization",
		"number", number,
		"hash", hash.Hex(),
		"receiptsRoot", receiptsRoot.Hex())
}

// Handle block finalization
func HandleFinalization(finalizedNumber uint64, finalizedHash common.Hash) error {
	processedBlocksMu.Lock()
	defer processedBlocksMu.Unlock()

	lastFinalizedBlockMu.Lock()
	defer lastFinalizedBlockMu.Unlock()

	// Only process if this is a new finalization
	if finalizedNumber <= lastFinalizedBlock {
		return nil
	}

	log.Info("Processing block finalization",
		"finalizedNumber", finalizedNumber,
		"finalizedHash", finalizedHash.Hex(),
		"previousFinalized", lastFinalizedBlock)

	// Collect blocks to be finalized
	var blocksToFinalize []BlockInfo
	for number, info := range processedBlocks {
		if number <= finalizedNumber && !info.Finalized {
			// Deep copy the block info
			blocksToFinalize = append(blocksToFinalize, *info)
			info.Finalized = true
		}
	}

	// Sort blocks by number for deterministic processing
	sort.Slice(blocksToFinalize, func(i, j int) bool {
		return blocksToFinalize[i].Number < blocksToFinalize[j].Number
	})

	// Notify bridge API about finalized blocks
	for _, info := range blocksToFinalize {
		if err := notifyBridgeFinalization(info); err != nil {
			log.Error("Failed to notify bridge about finalized block",
				"number", info.Number,
				"hash", info.Hash.Hex(),
				"err", err)
			continue
		}
		log.Info("Notified bridge about finalized block",
			"number", info.Number,
			"hash", info.Hash.Hex())
	}

	// Update last finalized block
	lastFinalizedBlock = finalizedNumber

	// Cleanup old blocks
	cleanupOldBlocks(finalizedNumber)

	return nil
}

// Notify bridge API about finalized block
func notifyBridgeFinalization(info BlockInfo) error {
	// Prepare request payload
	payload := struct {
		BlockNumber  string `json:"blockNumber"`
		ReceiptsRoot string `json:"receiptsRoot"`
	}{
		BlockNumber:  fmt.Sprintf("%d", info.Number),
		ReceiptsRoot: info.ReceiptsRoot.Hex(),
	}

	// Marshal payload
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal finalization payload: %w", err)
	}

	// Send to each enabled endpoint
	for _, endpoint := range info.Config.Endpoints {
		if !endpoint.Enabled {
			continue
		}

		// Create finalization request
		req, err := http.NewRequest("PATCH", endpoint.URL, bytes.NewBuffer(jsonData))
		if err != nil {
			log.Error("Failed to create finalization request", "err", err)
			continue
		}
		req.Header.Set("Content-Type", "application/json")

		// Send request
		client := &http.Client{Timeout: info.Config.RequestTimeout}
		resp, err := client.Do(req)
		if err != nil {
			log.Error("Failed to send finalization request", "err", err)
			continue
		}
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			log.Error("Bridge API returned non-200 status for finalization",
				"status", resp.StatusCode,
				"block", info.Number)
			continue
		}
	}

	return nil
}

// Cleanup old finalized blocks to prevent memory growth
func cleanupOldBlocks(finalizedNumber uint64) {
	const keepBlocks = 100 // Reduced from 1000 since we're only storing relevant blocks

	for number := range processedBlocks {
		if number+keepBlocks < finalizedNumber {
			delete(processedBlocks, number)
		}
	}
}

// ProcessReceipts processes block receipts and sends notifications for filtered transactions
func ProcessReceipts(receipts types.Receipts, header *types.Header, config BridgeConfig) {
	if receipts == nil || len(receipts) == 0 || header == nil {
		return
	}

	hasRelevantTransfers := false
	ctx := context.Background()

	// Iterate through block receipts first to check if we have any relevant transfers
	for _, receipt := range receipts {
		if receipt == nil || len(receipt.Logs) == 0 || receipt.BlockNumber == nil {
			continue
		}

		for _, logEntry := range receipt.Logs {
			// Check if this is a USDT transfer log
			if logEntry.Address != USDTContractAddress || len(logEntry.Topics) < 3 || logEntry.Topics[0] != TransferSignature {
				continue
			}

			// Extract transfer details
			from := common.BytesToAddress(logEntry.Topics[1].Bytes())
			to := common.BytesToAddress(logEntry.Topics[2].Bytes())

			// Check if the transfer involves a filtered address
			for _, filteredAddr := range FilteredAddresses {
				if from == filteredAddr || to == filteredAddr {
					hasRelevantTransfers = true
					amount := new(big.Int).SetBytes(logEntry.Data)

					// Send transfer to bridge
					if err := sendToBridge(ctx, USDTContractAddress, from, amount, receipt.BlockNumber.Uint64(),
						header.ReceiptHash, receipt.TransactionIndex, config); err != nil {
						log.Error("Failed to send filtered USDT transfer to bridge", "err", err)
					}

					log.Info("USDT transfer involving filtered address detected",
						"from", from.Hex(),
						"to", to.Hex(),
						"amount", amount.String(),
						"block", receipt.BlockNumber.Uint64(),
						"index", receipt.TransactionIndex)
					break
				}
			}
		}
	}

	// Only store block info if we found relevant transfers
	if hasRelevantTransfers {
		StoreBlockInfo(header.Number.Uint64(), header.Hash(), header.ReceiptHash, config)
	}
}
