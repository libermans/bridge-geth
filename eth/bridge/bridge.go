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

// ReceiptData represents a single receipt data to be sent to the bridge
type ReceiptData struct {
	Contract     string `json:"contract"`
	Owner        string `json:"owner"`
	Amount       string `json:"amount"`
	ReceiptIndex string `json:"receiptIndex"`
}

// BlockRequest represents a block with receipts to be sent to the bridge service
type BlockRequest struct {
	BlockNumber  string        `json:"blockNumber"`
	OriginChain  string        `json:"originChain"`
	ReceiptsRoot string        `json:"receiptsRoot"`
	Receipts     []ReceiptData `json:"receipts"`
}

// The USDT contract address on Ethereum mainnet
var USDTContractAddress = common.HexToAddress("0xdAC17F958D2ee523a2206206994597C13D831ec7")

// Known Binance addresses (add more as needed)
var FilteredAddresses = []common.Address{
	//common.HexToAddress("0x403962F6323e1F1B8A9238E9C07218f841889765"), // Binance 13
	//common.HexToAddress("0x28C6c06298d514Db089934071355E5743bf21d60"), // Binance 14
	//common.HexToAddress("0x21a31Ee1afC51d94C2eFcCAa2092aD1028285549"), // Binance 15
	//common.HexToAddress("0xDFd5293D8e347dFe59E90eFd55b2956a1343963d"), // Binance 16
	//common.HexToAddress("0x56Eddb7aa87536c09CCc2793473599fD21A8b17F"), // Binance 17
	common.HexToAddress("0x9696f59E4d72E237BE84fFD425DCaD154Bf96976"), // Binance 18
	//common.HexToAddress("0x21a31Ee1afC51d94C2eFcCAa2092aD1028285549"), // Binance Cold Wallet
	//common.HexToAddress("0xDFd5293D8e347dFe59E90eFd55b2956a1343963d"), // Binance Hot Wallet
	// Add more known Binance addresses here
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

// sendBlockToBridge sends block details with filtered receipts to all configured bridge endpoints
func sendBlockToBridge(ctx context.Context, blockNum uint64, receiptsRoot common.Hash,
	filteredReceipts []ReceiptData, config BridgeConfig) error {
	// Prepare request payload using the specified format
	payload := BlockRequest{
		BlockNumber:  fmt.Sprintf("%d", blockNum),
		OriginChain:  "ethereum",
		ReceiptsRoot: receiptsRoot.Hex(),
		Receipts:     filteredReceipts,
	}

	// Marshal the payload to JSON
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal JSON payload: %w", err)
	}

	// Send to each enabled endpoint
	for _, endpoint := range config.Endpoints {
		if !endpoint.Enabled {
			continue
		}

		// Process each endpoint sequentially
		err := sendToSingleEndpoint(ctx, endpoint.URL, jsonData, config.RequestTimeout)
		if err != nil {
			log.Error("Failed to send block to bridge endpoint",
				"url", endpoint.URL,
				"block", blockNum,
				"err", err)
		} else {
			log.Info("Successfully sent block to bridge endpoint",
				"url", endpoint.URL,
				"block", blockNum,
				"receiptsRoot", receiptsRoot.Hex(),
				"receiptCount", len(filteredReceipts))
		}
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

// ProcessBlocks processes blocks and sends all blocks to the bridge API
// If the block contains filtered transfers, those receipts are included in the request
func ProcessBlocks(receipts types.Receipts, header *types.Header, config BridgeConfig) {
	if header == nil {
		return
	}

	blockNum := header.Number.Uint64()
	ctx := context.Background()

	log.Info("Processing block",
		"number", blockNum,
		"hash", header.Hash().Hex(),
		"receipts_count", len(receipts))

	// This will hold the filtered receipts
	var filteredReceipts []ReceiptData

	// If the block has receipts, filter them for relevant transfers
	if len(receipts) > 0 {
		// Iterate through block receipts to collect relevant transfers
		for i, receipt := range receipts {
			if receipt == nil || len(receipt.Logs) == 0 {
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
					if to == filteredAddr {
						amount := new(big.Int).SetBytes(logEntry.Data)

						// Add this receipt to our filtered list
						receiptData := ReceiptData{
							Contract:     USDTContractAddress.Hex(),
							Owner:        from.Hex(),
							Amount:       amount.String(),
							ReceiptIndex: fmt.Sprintf("%d", i), // Use the receipt's index in the receipts array
						}

						filteredReceipts = append(filteredReceipts, receiptData)

						log.Info("USDT transfer involving filtered address detected",
							"from", from.Hex(),
							"to", to.Hex(),
							"amount", amount.String(),
							"block", blockNum,
							"index", i)
						break
					}
				}
			}
		}
	}

	log.Info("Block processing complete",
		"number", blockNum,
		"filtered_receipts", len(filteredReceipts))

	// Send block to bridge (even if no relevant receipts were found)
	if err := sendBlockToBridge(ctx, blockNum, header.ReceiptHash, filteredReceipts, config); err != nil {
		log.Error("Failed to send block to bridge",
			"block", blockNum,
			"receiptsRoot", header.ReceiptHash.Hex(),
			"err", err)
	}
}
