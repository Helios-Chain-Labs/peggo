package cosmos

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"

	cosmoscodec "github.com/cosmos/cosmos-sdk/codec"
	cosmoscdctypes "github.com/cosmos/cosmos-sdk/codec/types"
	cosmoscrypto "github.com/cosmos/cosmos-sdk/crypto"
	"github.com/cosmos/cosmos-sdk/crypto/keyring"
	//"github.com/cosmos/cosmos-sdk/types/bech32"
	cosmostypes "github.com/cosmos/cosmos-sdk/types"
	"github.com/pkg/errors"

	"github.com/Helios-Chain-Labs/sdk-go/chain/codec"
	"github.com/Helios-Chain-Labs/sdk-go/chain/crypto/ethsecp256k1"
	"github.com/Helios-Chain-Labs/sdk-go/chain/crypto/hd"
)

const (
	DefaultKeyName = "validator"
)

type KeyringConfig struct {
	KeyringDir,
	KeyringAppName,
	KeyringBackend,
	KeyFrom,
	KeyPassphrase,
	PrivateKey string
	UseLedger bool
}

func (cfg KeyringConfig) withPrivateKey() bool {
	return len(cfg.PrivateKey) > 0
}

type Keyring struct {
	keyring.Keyring

	Addr cosmostypes.AccAddress
}

func NewKeyring(cfg KeyringConfig) (Keyring, error) {
	if cfg.withPrivateKey() {
		return newInMemoryKeyring(cfg)
	}

	return newKeyringFromDir(cfg)
}

// func GetFromBech32(bech32str, prefix string) (cosmostypes.AccAddress, error) {
// 	if len(bech32str) == 0 {
// 		return nil, errors.New("KKKK")
// 	}

// 	hrp, bz, err := bech32.DecodeAndConvert(bech32str)
// 	if err != nil {
// 		return nil, err
// 	}
// 	if hrp == "" {
// 		return nil, errors.New("invalid hrp")
// 	}

// 	// bstr, err := bech32.ConvertAndEncode(prefix, bz)

// 	// if err != nil {
// 	// 	return nil, err
// 	// }

// 	// hrp2, bz2, err := bech32.DecodeAndConvert(bstr)
// 	// if err != nil {
// 	// 	return nil, err
// 	// }
// 	// if hrp2 == "" {
// 	// 	return nil, errors.New("invalid hrp")
// 	// }

// 	return cosmostypes.AccAddress(bz), nil
// }

func newInMemoryKeyring(cfg KeyringConfig) (Keyring, error) {
	if cfg.UseLedger {
		return Keyring{}, errors.New("cannot use both private key and Ledger")
	}

	pk := cfg.PrivateKey
	if strings.HasPrefix(pk, "0x") {
		pk = pk[2:]
	}

	pkRaw, err := hex.DecodeString(pk)
	if err != nil {
		return Keyring{}, errors.Wrap(err, "invalid private key")
	}

	var (
		cosmosPK   = &ethsecp256k1.PrivKey{Key: pkRaw}
		cosmosAddr = cosmostypes.AccAddress(cosmosPK.PubKey().Address())
		keyName    = DefaultKeyName
	)

	// cosmosAddr, err := GetFromBech32(cfg.KeyFrom, "helios")
	// // cosmosAddr, err := cosmostypes.AccAddressFromBech32(cfg.KeyFrom)
	// if err != nil {
	// 	return Keyring{}, errors.Wrap(err, "failed to import private key")
	// }

	if len(cfg.KeyFrom) > 0 {
		from, err := cosmostypes.AccAddressFromBech32(cfg.KeyFrom)
		if err != nil {
			keyName = cfg.KeyFrom // use it as key name
		} else if !bytes.Equal(from.Bytes(), cosmosAddr.Bytes()) {
			return Keyring{}, errors.Errorf("expected account address %s but got %s from the private key", from.String(), cosmosAddr.String())
		}
	}

	// Create a temporary in-mem keyring for cosmosPK.
	// Allows to init Context when the key has been provided in plaintext and parsed.
	tmpPhrase := randPhrase(64)
	armored := cosmoscrypto.EncryptArmorPrivKey(cosmosPK, tmpPhrase, cosmosPK.Type())

	kr := keyring.NewInMemory(Codec(), hd.EthSecp256k1Option())
	if err := kr.ImportPrivKey(keyName, armored, tmpPhrase); err != nil {
		return Keyring{}, errors.Wrap(err, "failed to import private key")
	}

	if err != nil {
		return Keyring{}, errors.Wrap(err, "failed to initialize cosmos keyring")
	}

	k := Keyring{
		Keyring: kr,
		Addr:    cosmosAddr,
	}

	return k, nil
}

func newKeyringFromDir(cfg KeyringConfig) (Keyring, error) {
	if len(cfg.KeyFrom) == 0 {
		return Keyring{}, errors.New("insufficient cosmos details provided")
	}

	keyringDir := cfg.KeyringDir
	if !filepath.IsAbs(keyringDir) {
		dir, err := filepath.Abs(keyringDir)
		if err != nil {
			return Keyring{}, errors.Wrap(err, "failed to get absolute path of keyring dir")
		}

		keyringDir = dir
	}

	var reader io.Reader = os.Stdin
	if len(cfg.KeyPassphrase) > 0 {
		reader = newPassReader(cfg.KeyPassphrase)
	}

	kr, err := keyring.New(
		cfg.KeyringAppName,
		cfg.KeyringBackend,
		keyringDir,
		reader,
		Codec(),
		hd.EthSecp256k1Option(),
	)

	if err != nil {
		return Keyring{}, errors.Wrap(err, "failed to initialize cosmos keyring")
	}

	var keyRecord *keyring.Record
	if cosmosAddr, err := cosmostypes.AccAddressFromBech32(cfg.KeyFrom); err != nil {
		r, err := kr.Key(cfg.KeyFrom)
		if err != nil {
			return Keyring{}, err
		}

		keyRecord = r
	} else {
		r, err := kr.KeyByAddress(cosmosAddr)
		if err != nil {
			return Keyring{}, err
		}

		keyRecord = r
	}

	switch keyRecord.GetType() {
	case keyring.TypeLocal:
		// kb has a key and it's totally usable
		addr, err := keyRecord.GetAddress()
		if err != nil {
			return Keyring{}, errors.Wrap(err, "failed to get address from key record")
		}

		k := Keyring{
			Keyring: kr,
			Addr:    addr,
		}

		return k, nil
	case keyring.TypeLedger:
		// the kb stores references to ledger keys, so we must explicitly
		// check that. kb doesn't know how to scan HD keys - they must be added manually before
		if !cfg.UseLedger {
			return Keyring{}, errors.Errorf("key %s is a Ledger reference, enable Ledger option", keyRecord.Name)
		}

		addr, err := keyRecord.GetAddress()
		if err != nil {
			return Keyring{}, errors.Wrap(err, "failed to get address from key record")

		}

		k := Keyring{
			Keyring: kr,
			Addr:    addr,
		}

		return k, nil
	default:
		return Keyring{}, errors.Errorf("unsupported key type: %s", keyRecord.GetType())
	}
}

func Codec() cosmoscodec.Codec {
	iRegistry := cosmoscdctypes.NewInterfaceRegistry()
	codec.RegisterInterfaces(iRegistry)
	codec.RegisterLegacyAminoCodec(cosmoscodec.NewLegacyAmino())

	return cosmoscodec.NewProtoCodec(iRegistry)
}

func randPhrase(size int) string {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		panic("rand failed")
	}

	return string(buf)
}

var _ io.Reader = &passReader{}

type passReader struct {
	pass string
	buf  *bytes.Buffer
}

func newPassReader(pass string) io.Reader {
	return &passReader{
		pass: pass,
		buf:  new(bytes.Buffer),
	}
}

func (r *passReader) Read(p []byte) (n int, err error) {
	n, err = r.buf.Read(p)
	if err == io.EOF || n == 0 {
		r.buf.WriteString(r.pass + "\n")

		n, err = r.buf.Read(p)
	}

	return
}
