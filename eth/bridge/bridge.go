package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/log"
)

// Add these variables at the package level
var (
	// New variables for block tracking
	processedBlocks   = make(map[uint64]*BlockInfo) // map[blockNumber]*BlockInfo
	processedBlocksMu sync.RWMutex

	config     *ethconfig.Config
	configOnce sync.Once
)

func SetConfig(incomingConfig *ethconfig.Config) {
	configOnce.Do(func() {
		config = incomingConfig
	})
}

func GetConfig() *ethconfig.Config {
	return config
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

// TransferSignature is the event signature for ERC20 Transfer events
var TransferSignature = crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))

// Add new types and variables for block tracking
type BlockInfo struct {
	Number       uint64
	Hash         common.Hash
	ReceiptsRoot common.Hash
	Finalized    bool
	HasTransfers bool // Indicates if this block has relevant transfers
}

// sendBlockToBridge sends block details with filtered receipts to all configured bridge endpoints
func sendBlockToBridge(ctx context.Context, blockNum uint64, receiptsRoot common.Hash,
	filteredReceipts []ReceiptData) error {
	config := GetConfig()
	if config == nil || len(config.BridgeEndpoints) == 0 {
		return nil
	}

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

	// Send to each endpoint
	for _, endpoint := range config.BridgeEndpoints {
		// Process each endpoint sequentially
		err := sendToSingleEndpoint(ctx, endpoint, jsonData, config.BridgeTimeout)
		if err != nil {
			log.Error("Failed to send block to bridge endpoint",
				"url", endpoint,
				"block", blockNum,
				"err", err)
		} else {
			log.Info("Successfully sent block to bridge endpoint",
				"url", endpoint,
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
	client := &http.Client{Timeout: timeout}
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
func StoreBlockInfo(number uint64, hash, receiptsRoot common.Hash) {
	processedBlocksMu.Lock()
	defer processedBlocksMu.Unlock()

	processedBlocks[number] = &BlockInfo{
		Number:       number,
		Hash:         hash,
		ReceiptsRoot: receiptsRoot,
		Finalized:    false,
		HasTransfers: false,
	}
	log.Debug("Stored block info for future finalization",
		"number", number,
		"hash", hash.Hex(),
		"receiptsRoot", receiptsRoot.Hex())
}

// ProcessBlocks processes blocks and sends all blocks to the bridge API
// If the block contains filtered transfers, those receipts are included in the request
func ProcessBlocks(receipts types.Receipts, header *types.Header) error {
	config := GetConfig()
	if config == nil || len(config.BridgeEndpoints) == 0 {
		// No endpoints configured, skip processing
		return nil
	}

	if header == nil {
		return nil
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

				if to == config.BridgeContract {
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

	log.Info("Block processing complete",
		"number", blockNum,
		"filtered_receipts", len(filteredReceipts))

	// Send block to bridge (even if no relevant receipts were found)
	if err := sendBlockToBridge(ctx, blockNum, header.ReceiptHash, filteredReceipts); err != nil {
		return err
	}

	return nil
}
