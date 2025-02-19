package util

import (
	"bytes"
	sdkmath "cosmossdk.io/math"
	"fmt"
	"math/big"
	"regexp"
	"strings"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/pkg/errors"
)

const (
	// PeggyDenomPrefix indicates the prefix for all assests minted by this module
	PeggyDenomPrefix = "helios"

	// PeggyDenomSeperator is the seperator for peggy denoms
	PeggyDenomSeperator = "/"

	ETHContractAddressLen = 42

	// PeggyDenomLen is the length of the denoms generated by the peggy module
	PeggyDenomLen = len(PeggyDenomPrefix) + len(PeggyDenomSeperator) + ETHContractAddressLen
)

// EthAddrLessThan migrates the Ethereum address less than function
func EthAddrLessThan(e, o string) bool {
	return bytes.Compare([]byte(e)[:], []byte(o)[:]) == -1
}

var hexRx = regexp.MustCompile("^0x[0-9a-fA-F]{40}$")

// ValidateEthAddress validates the ethereum address string
func ValidateEthAddress(a string) error {
	if len(a) == 0 {
		return errors.Errorf("empty address provided")
	}
	if !hexRx.MatchString(a) {
		return errors.Errorf("address(%s) doesn't match Hex-encoding regex", a)
	}
	if len(a) != ETHContractAddressLen {
		return errors.Errorf("address(%s) of the wrong length exp(%d) actual(%d)", a, ETHContractAddressLen, len(a))
	}

	return nil
}

type ERC20Token struct {
	Amount   *big.Int       `json:"amount"`
	Contract common.Address `json:"contract,omitempty"`
}

// NewERC20Token returns a new instance of an ERC20
func NewERC20Token(amount uint64, contract string) *ERC20Token {
	return &ERC20Token{
		Amount:   new(big.Int).SetUint64(amount),
		Contract: common.HexToAddress(contract),
	}
}

// PeggyCoin returns the peggy representation of the ERC20
func (e *ERC20Token) PeggyCoin() sdk.Coin {
	return sdk.NewCoin(fmt.Sprintf("%s/%s", PeggyDenomPrefix, e.Contract), sdkmath.NewIntFromBigInt(e.Amount))
}

// ValidateBasic permforms stateless validation
func (e *ERC20Token) ValidateBasic() error {
	// TODO: Validate all the things
	return nil
}

// Add adds one ERC20 to another
func (e *ERC20Token) Add(o *ERC20Token) *ERC20Token {
	if !bytes.Equal(e.Contract.Bytes(), o.Contract.Bytes()) {
		panic("invalid contract address")
	}

	return &ERC20Token{
		Amount:   new(big.Int).Add(e.Amount, o.Amount),
		Contract: e.Contract,
	}
}

// ERC20FromPeggyCoin returns the ERC20 representation of a given peggy coin
func ERC20FromPeggyCoin(v sdk.Coin) (*ERC20Token, error) {
	contract, err := ValidatePeggyCoin(v)
	if err != nil {
		return nil, errors.Errorf("%s isn't a valid peggy coin: %s", v.String(), err)
	}

	return &ERC20Token{
		Contract: common.HexToAddress(contract),
		Amount:   v.Amount.BigInt(),
	}, nil
}

// ValidatePeggyCoin returns true if a coin is a peggy representation of an ERC20 token
func ValidatePeggyCoin(v sdk.Coin) (string, error) {
	parts := strings.Split(v.Denom, PeggyDenomSeperator)
	if len(parts) < 2 {
		return "", errors.Errorf("denom(%s) not valid, fewer seperators(%s) than expected", v.Denom, PeggyDenomSeperator)
	}

	denomPrefix := parts[0]
	contract := parts[1]
	err := ValidateEthAddress(contract)

	switch {
	case len(parts) != 2:
		return "", errors.Errorf("denom(%s) not valid, more seperators(%s) than expected", v.Denom, PeggyDenomSeperator)
	case denomPrefix != PeggyDenomPrefix:
		return "", errors.Errorf("denom prefix(%s) not equal to expected(%s)", denomPrefix, PeggyDenomPrefix)
	case err != nil:
		return "", errors.Errorf("error(%s) validating ethereum contract address", err)
	case len(v.Denom) != PeggyDenomLen:
		return "", errors.Errorf("len(denom)(%d) not equal to PeggyDenomLen(%d)", len(v.Denom), PeggyDenomLen)
	default:
		return contract, nil
	}
}
