package main

import (
	"bytes"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/Dexconv/go-bitcoind"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/jmoiron/sqlx"
	"github.com/kelseyhightower/envconfig"
	"github.com/mitchellh/go-homedir"
	"github.com/rs/zerolog"
	_ "modernc.org/sqlite"
)

type Settings struct {
	BitcoinChain string `envconfig:"BITCOIN_CHAIN" default:"mainnet"`

	BitcoindHost     string `envconfig:"BITCOIND_HOST" default:"127.0.0.1"`
	BitcoindPort     int    `envconfig:"BITCOIND_PORT"`
	BitcoindUser     string `envconfig:"BITCOIND_USER"`
	BitcoindPassword string `envconfig:"BITCOIND_PASSWORD"`

	ConfigPath string `json:"CONFIG_PATH" default:"~/.config/openchain/guardian"`
}

var chainParams = &chaincfg.MainNetParams

const CANONICAL_AMOUNT = 738

// configs
var (
	s          Settings
	configDir  string
	configPath string
	sqlitedsn  string
)

// global instances
var (
	log = zerolog.New(os.Stderr).Output(zerolog.ConsoleWriter{Out: os.Stderr})
	db  *sqlx.DB
	bc  *bitcoind.Bitcoind
)

// runtime global values
var (
	chainKey          *btcec.PrivateKey
	chainPubKeyHash   []byte
	chainPubKeyScript []byte
	chainHasStarted   bool
)

type Config struct {
	ChainKey string `json:"chainkey"`
}

func main() {
	zerolog.SetGlobalLevel(zerolog.DebugLevel)

	// environment variables
	err := envconfig.Process("", &s)
	if err != nil {
		log.Fatal().Err(err).Msg("couldn't process envconfig.")
		return
	}

	// configs
	switch s.BitcoinChain {
	case "mainnet":
		chainParams = &chaincfg.MainNetParams
	case "testnet":
		chainParams = &chaincfg.TestNet3Params
	case "signet":
		chainParams = &chaincfg.SigNetParams
	case "regtest":
		chainParams = &chaincfg.RegressionNetParams
	}

	if s.BitcoindPort == 0 {
		switch s.BitcoinChain {
		case "mainnet":
			s.BitcoindPort = 8332
		case "testnet":
			s.BitcoindPort = 18332
		case "signet":
			s.BitcoindPort = 38332
		case "regtest":
			s.BitcoindPort = 18443
		}
	}

	// paths
	configDir, _ = homedir.Expand(s.ConfigPath)
	configPath = path.Join(configDir, "keys.json")
	sqlitedsn = path.Join(configDir, "db.sqlite")
	os.MkdirAll(configDir, 0700)

	// create tables
	db = sqlx.MustOpen("sqlite", sqlitedsn)
	db.Exec(`
        CREATE TABLE kv (
          key TEXT PRIMARY KEY,
          value TEXT
        );
        CREATE TABLE chain_block_tx (
          idx INT,
          txid TEXT PRIMARY KEY
        );
    `)

	// check if the chain has started
	db.Get(&chainHasStarted, "SELECT true FROM chain_block_tx")

	// start bitcoind RPC
	if bitcoindRPC, err := bitcoind.New(s.BitcoindHost, s.BitcoindPort, s.BitcoindUser, s.BitcoindPassword, false); err != nil {
		log.Fatal().Err(err).Msg("can't connect to bitcoind")
		return
	} else {
		bc = bitcoindRPC
	}

	// init config and keys
	if b, err := ioutil.ReadFile(configPath); err != nil {
		if strings.HasSuffix(err.Error(), "no such file or directory") {
			// create a new private key
			log.Info().Str("path", configPath).Msg("creating private key and storing it")
			if chainKey, err = btcec.NewPrivateKey(); err != nil {
				log.Fatal().Err(err).Msg("error creating chain key")
				return
			}

			jconfig, _ := json.Marshal(Config{
				ChainKey: hex.EncodeToString(chainKey.Serialize()),
			})
			if err := os.WriteFile(configPath, jconfig, 0600); err != nil {
				log.Fatal().Err(err).Msg("error saving config key")
				return
			}
		} else {
			log.Fatal().Err(err).Msg("error reading config file")
			return
		}
	} else {
		var config Config
		json.Unmarshal(b, &config)
		c, _ := hex.DecodeString(config.ChainKey)
		chainKey, _ = btcec.PrivKeyFromBytes(c)

		if chainKey == nil {
			log.Fatal().Err(err).Msg("error parsing config json")
		}
	}

	// if we don't have any block data in the database,
	// determine that we are starting now so we don't have to scan the entire chain
	if count, err := bc.GetBlockCount(); err != nil {
		log.Fatal().Err(err).Msg("error getting block count")
		return
	} else {
		// we just do this, it will fail if the key is already set
		db.Exec("INSERT INTO kv VALUES ('blockheight', $1)", count)
	}

	// print addresses
	chainPubKeyHash = btcutil.Hash160(chainKey.PubKey().SerializeCompressed())
	chainAddress, _ := btcutil.NewAddressWitnessPubKeyHash(chainPubKeyHash, chainParams)
	log.Debug().Str("address", chainAddress.String()).Msg("")
	chainPubKeyScript = append([]byte{0, 20}, chainPubKeyHash...)

	// handle commands
	http.HandleFunc("/", handleInfo)
	go http.ListenAndServe(":10738", nil)

	// inspect blocks
	inspectBlocks()
}

func inspectBlocks() {
	var currentBlock uint64
	if err := db.Get(&currentBlock, "SELECT value FROM kv WHERE key = 'blockheight'"); err != nil {
		log.Fatal().Err(err).Msg("failed to get current block from db")
		return
	}

	// start at the next
	currentBlock++

	for {
		log.Debug().Uint64("height", currentBlock).Msg("inspecting block")

		// get block from bitcoind
		hash, err := bc.GetBlockHash(currentBlock)
		if err != nil && strings.HasPrefix(err.Error(), "-8:") {
			time.Sleep(1 * time.Minute)
			continue
		} else if err != nil {
			log.Fatal().Err(err).Uint64("height", currentBlock).Msg("no block")
			return
		}

		blockHex, err := bc.GetRawBlock(hash)
		if err != nil {
			log.Fatal().Err(err).Str("hash", hash).Msg("no block")
			return
		}

		// process block and save it
		if err := processBlock(currentBlock, blockHex); err != nil {
			log.Fatal().Err(err).Str("hash", hash).Uint64("height", currentBlock).Msg("failed to process block")
			return
		}

		// jump to the next block
		currentBlock++
	}
}

func processBlock(blockHeight uint64, blockHex string) error {
	txn := db.MustBegin()
	defer txn.Rollback()

	raw, err := hex.DecodeString(blockHex)
	if err != nil {
		return fmt.Errorf("block hex broken: %w", err)
	}

	block, err := btcutil.NewBlockFromBytes(raw)
	if err != nil {
		return fmt.Errorf("failed to parse block: %w", err)
	}

	for _, tx := range block.Transactions() {
		// chain tx relevant inputs and outputs are always on the first index
		input := tx.MsgTx().TxIn[0]
		output := tx.MsgTx().TxOut[0]

		if bytes.HasSuffix(output.PkScript, chainPubKeyHash) && len(output.PkScript) == 22 {
			// check if the chain has moved
			var index uint64
			if err := txn.Get(
				&index,
				"SELECT idx + 1 FROM chain_block_tx WHERE txid = $1",
				input.PreviousOutPoint.Hash.String(),
			); err == sql.ErrNoRows && chainHasStarted {
				// this was just a dummy output that doesn't reference the chain tx,
				// just ignore it
				log.Warn().Str("txid", tx.Hash().String()).
					Msg("got tx but not part of the canonical chain")
				continue
			} else if err == sql.ErrNoRows && !chainHasStarted {
				// the chain hasn't started yet, so we will take the first output we
				// can get that matches the canonical amount and it will be the
				// genesis block
				if output.Value != CANONICAL_AMOUNT {
					log.Warn().
						Str("txid", tx.Hash().String()).
						Int64("sats", output.Value).
						Msg("got tx but can't be the genesis since the amount is wrong")
					continue
				} else {
					// it's ok, we will use this one, just proceed
					index = 0
				}
			} else if err != nil {
				return fmt.Errorf("failed to read chain_block_tx: %w", err)
			}

			if _, err := txn.Exec(
				"INSERT INTO chain_block_tx (idx, txid) VALUES ($1, $2)",
				index+1, tx.Hash().String(),
			); err != nil {
				if strings.HasPrefix(err.Error(), "constraint failed: UNIQUE constraint") {
					// no problem, just skip
					continue
				}

				return fmt.Errorf("failed to insert into chain_block_tx: %w", err)
			}

			log.Info().Str("txid", tx.Hash().String()).Msg("new openchain tip found")
		}
	}

	if _, err := txn.Exec("UPDATE kv SET value = $1 WHERE key = 'blockheight'", blockHeight); err != nil {
		return fmt.Errorf("failed to update blockheight: %w", err)
	}

	return txn.Commit()
}

func handleInfo(w http.ResponseWriter, r *http.Request) {
	var current struct {
		TxCount uint64 `db:"idx" json:"tx_count,omitempty"`
		TipTx   string `db:"txid" json:"tip_tx,omitempty"`
	}
	if err := db.Get(
		&current,
		"SELECT idx, txid FROM chain_block_tx ORDER BY idx DESC LIMIT 1",
	); err != nil {
		log.Error().Err(err).Msg("error fetching txid tip on / -- is there a genesis tx registered on the db?")
		w.WriteHeader(501)
		return
	}

	// make the next presigned tx
	tip, _ := chainhash.NewHashFromStr(current.TipTx)
	packet, _ := psbt.New(
		[]*wire.OutPoint{{Hash: *tip, Index: 0}},
		[]*wire.TxOut{{Value: CANONICAL_AMOUNT, PkScript: chainPubKeyScript}},
		2,
		0,
		[]uint32{1},
	)

	// sign previous chain tip
	// we will use this txscript thing to build the signature for us, then we will take it and apply to the psbt
	witnessProgram, _ := txscript.NewScriptBuilder().AddOp(txscript.OP_0).AddData(chainPubKeyHash).Script()
	fetcher := txscript.NewCannedPrevOutputFetcher(chainPubKeyScript, CANONICAL_AMOUNT)
	sigHashes := txscript.NewTxSigHashes(packet.UnsignedTx, fetcher)
	signature, err := txscript.RawTxInWitnessSignature(
		packet.UnsignedTx,
		sigHashes,
		0,
		CANONICAL_AMOUNT,
		witnessProgram,
		txscript.SigHashSingle|txscript.SigHashAnyOneCanPay,
		chainKey,
	)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to compute")
		return
	}

	upd, _ := psbt.NewUpdater(packet)
	upd.AddInWitnessUtxo(wire.NewTxOut(CANONICAL_AMOUNT, chainPubKeyScript), 0)
	upd.AddInSighashType(txscript.SigHashSingle|txscript.SigHashAnyOneCanPay, 0)

	if _, err := upd.Sign(
		0,
		signature,
		chainKey.PubKey().SerializeCompressed(),
		nil,
		nil,
	); err != nil {
		log.Fatal().Err(err).Msg("failed to add signature to psbt")
	}
	if err := psbt.Finalize(upd.Upsbt, 0); err != nil {
		log.Fatal().Err(err).Msg("failed to finalize psbt input")
	}

	upd.Upsbt.UnsignedTx.TxIn[0].Witness = [][]byte{signature, chainKey.PubKey().SerializeCompressed()}

	// finally build the next output
	t := &bytes.Buffer{}
	upd.Upsbt.UnsignedTx.Serialize(t)

	b64psbt, _ := packet.B64Encode()

	next := struct {
		Raw  string `json:"raw"`
		PSBT string `json:"psbt"`
	}{
		hex.EncodeToString(t.Bytes()),
		b64psbt,
	}

	// and return the response
	json.NewEncoder(w).Encode(struct {
		Current interface{} `json:"current"`
		Next    interface{} `json:"next"`
	}{current, next})
}
