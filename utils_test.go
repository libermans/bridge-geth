package gonkaopenai

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcutil/bech32"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/assert"
	"golang.org/x/crypto/ripemd160"
)

func TestCrossChainAddressDerivation(t *testing.T) {
	// Test private key (this is a test key, never use in production)
	privateKeyHex := "0b0cfdb2bac15a7e35272617c928708208d7b394ccbcdb38074fec8f13a4ab8c"

	// 1. Generate Ethereum address
	keyBytes, err := hex.DecodeString(privateKeyHex)
	assert.NoError(t, err)

	privateKey, err := crypto.ToECDSA(keyBytes)
	assert.NoError(t, err)

	ethAddress := crypto.PubkeyToAddress(privateKey.PublicKey)
	t.Logf("Ethereum address: %s", ethAddress.Hex())

	// 2. Generate Cosmos address using the same steps as GonkaAddress
	// Compress the public key
	pub := crypto.CompressPubkey(&privateKey.PublicKey)

	// SHA256 hash of the compressed public key
	sha := sha256.Sum256(pub)

	// RIPEMD160 hash of the SHA256 hash
	hasher := ripemd160.New()
	hasher.Write(sha[:])
	ripe := hasher.Sum(nil)

	// Convert to 5-bit words for bech32 encoding
	five, err := bech32.ConvertBits(ripe[:], 8, 5, true)
	assert.NoError(t, err)

	// Encode with bech32 using the chain prefix
	prefix := "gonka" // Using the same prefix as in GonkaAddress
	cosmosAddress, err := bech32.Encode(prefix, five)
	assert.NoError(t, err)
	t.Logf("Cosmos address: %s", cosmosAddress)

	// 3. Sign a message using Ethereum's signing method
	message := []byte("Hello, cross-chain world!")
	hash := crypto.Keccak256Hash(message)
	signature, err := crypto.Sign(hash.Bytes(), privateKey)
	assert.NoError(t, err)

	// 4. Recover public key from signature
	recoveredPubKey, err := crypto.SigToPub(hash.Bytes(), signature)
	assert.NoError(t, err)

	// 5. Generate Cosmos address from recovered public key using the same steps
	// Compress the public key
	recoveredPub := crypto.CompressPubkey(recoveredPubKey)

	// SHA256 hash of the compressed public key
	recoveredSha := sha256.Sum256(recoveredPub)

	// RIPEMD160 hash of the SHA256 hash
	recoveredHasher := ripemd160.New()
	recoveredHasher.Write(recoveredSha[:])
	recoveredRipe := recoveredHasher.Sum(nil)

	// Convert to 5-bit words for bech32 encoding
	recoveredFive, err := bech32.ConvertBits(recoveredRipe[:], 8, 5, true)
	assert.NoError(t, err)

	// Encode with bech32 using the chain prefix
	recoveredCosmosAddress, err := bech32.Encode(prefix, recoveredFive)
	assert.NoError(t, err)

	t.Logf("Recovered Cosmos address: %s", recoveredCosmosAddress)

	// 6. Verify addresses match
	assert.Equal(t, cosmosAddress, recoveredCosmosAddress, "Original and recovered Cosmos addresses should match")

	// 7. Verify Ethereum address from recovered key
	recoveredEthAddress := crypto.PubkeyToAddress(*recoveredPubKey)
	assert.Equal(t, ethAddress, recoveredEthAddress, "Original and recovered Ethereum addresses should match")
}
