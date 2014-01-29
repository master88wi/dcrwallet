/*
 * Copyright (c) 2013, 2014 Conformal Systems LLC <info@conformal.com>
 *
 * Permission to use, copy, modify, and distribute this software for any
 * purpose with or without fee is hereby granted, provided that the above
 * copyright notice and this permission notice appear in all copies.
 *
 * THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
 * WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
 * MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
 * ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
 * WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
 * ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
 * OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
 */

package main

import (
	"errors"
	"github.com/conformal/btcjson"
	"github.com/conformal/btcutil"
	"github.com/conformal/btcwallet/wallet"
	"github.com/conformal/btcwire"
	"io/ioutil"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"strings"
	"sync"
	"time"
)

var (
	// ErrNoWallet describes an error where a wallet does not exist and
	// must be created first.
	ErrNoWallet = &WalletOpenError{
		Err: "wallet file does not exist",
	}

	// ErrNoUtxos describes an error where the wallet file was successfully
	// read, but the UTXO file was not.  To properly handle this error,
	// a rescan should be done since the wallet creation block.
	ErrNoUtxos = errors.New("utxo file cannot be read")

	// ErrNoTxs describes an error where the wallet and UTXO files were
	// successfully read, but the TX history file was not.  It is up to
	// the caller whether this necessitates a rescan or not.
	ErrNoTxs = errors.New("tx file cannot be read")

	cfg *config

	curBlock = struct {
		sync.RWMutex
		wallet.BlockStamp
	}{
		BlockStamp: wallet.BlockStamp{
			Height: int32(btcutil.BlockHeightUnknown),
		},
	}
)

// GetCurBlock returns the blockchain height and SHA hash of the most
// recently seen block.  If no blocks have been seen since btcd has
// connected, btcd is queried for the current block height and hash.
func GetCurBlock() (bs wallet.BlockStamp, err error) {
	curBlock.RLock()
	bs = curBlock.BlockStamp
	curBlock.RUnlock()
	if bs.Height != int32(btcutil.BlockHeightUnknown) {
		return bs, nil
	}

	bb, _ := GetBestBlock(CurrentRPCConn())
	if bb == nil {
		return wallet.BlockStamp{
			Height: int32(btcutil.BlockHeightUnknown),
		}, errors.New("current block unavailable")
	}

	hash, err := btcwire.NewShaHashFromStr(bb.Hash)
	if err != nil {
		return wallet.BlockStamp{
			Height: int32(btcutil.BlockHeightUnknown),
		}, err
	}

	curBlock.Lock()
	if bb.Height > curBlock.BlockStamp.Height {
		bs = wallet.BlockStamp{
			Height: bb.Height,
			Hash:   *hash,
		}
		curBlock.BlockStamp = bs
	}
	curBlock.Unlock()
	return bs, nil
}

// NewJSONID is used to receive the next unique JSON ID for btcd
// requests, starting from zero and incrementing by one after each
// read.
var NewJSONID = make(chan uint64)

// JSONIDGenerator sends incremental integers across a channel.  This
// is meant to provide a unique value for the JSON ID field for btcd
// messages.
func JSONIDGenerator(c chan uint64) {
	var n uint64
	for {
		c <- n
		n++
	}
}

func main() {
	// Initialize logging and setup deferred flushing to ensure all
	// outstanding messages are written on shutdown
	loggers := setLogLevel(defaultLogLevel)
	defer func() {
		for _, logger := range loggers {
			logger.Flush()
		}
	}()

	tcfg, _, err := loadConfig()
	if err != nil {
		os.Exit(1)
	}
	cfg = tcfg

	// Change the logging level if needed.
	if cfg.DebugLevel != defaultLogLevel {
		loggers = setLogLevel(cfg.DebugLevel)
	}

	if cfg.Profile != "" {
		go func() {
			listenAddr := net.JoinHostPort("", cfg.Profile)
			log.Infof("Profile server listening on %s", listenAddr)
			profileRedirect := http.RedirectHandler("/debug/pprof",
				http.StatusSeeOther)
			http.Handle("/", profileRedirect)
			log.Errorf("%v", http.ListenAndServe(listenAddr, nil))
		}()
	}

	// Check and update any old file locations.
	updateOldFileLocations()

	// Open all account saved to disk.
	OpenAccounts()

	// Read CA file to verify a btcd TLS connection.
	cafile, err := ioutil.ReadFile(cfg.CAFile)
	if err != nil {
		log.Errorf("cannot open CA file: %v", err)
		os.Exit(1)
	}

	// Start account disk syncer goroutine.
	go AccountDiskSyncer()

	go func() {
		s, err := newServer(cfg.SvrListeners)
		if err != nil {
			log.Errorf("Unable to create HTTP server: %v", err)
			os.Exit(1)
		}

		// Start HTTP server to listen and send messages to frontend and btcd
		// backend.  Try reconnection if connection failed.
		s.Start()
	}()

	// Begin generating new IDs for JSON calls.
	go JSONIDGenerator(NewJSONID)

	// Begin maintanence goroutines.
	go SendBeforeReceiveHistorySync(SendTxHistSyncChans.add,
		SendTxHistSyncChans.done,
		SendTxHistSyncChans.remove,
		SendTxHistSyncChans.access)
	go StoreNotifiedMempoolRecvTxs(NotifiedRecvTxChans.add,
		NotifiedRecvTxChans.remove,
		NotifiedRecvTxChans.access)
	go NotifyMinedTxSender(NotifyMinedTx)
	go NotifyBalanceSyncer(NotifyBalanceSyncerChans.add,
		NotifyBalanceSyncerChans.remove,
		NotifyBalanceSyncerChans.access)

	updateBtcd := make(chan *BtcdRPCConn)
	go func() {
		// Create an RPC connection and close the closed channel.
		//
		// It might be a better idea to create a new concrete type
		// just for an always disconnected RPC connection and begin
		// with that.
		btcd := NewBtcdRPCConn(nil)
		close(btcd.closed)

		// Maintain the current btcd connection.  After reconnects,
		// the current connection should be updated.
		for {
			select {
			case conn := <-updateBtcd:
				btcd = conn

			case access := <-accessRPC:
				access.rpc <- btcd
			}
		}
	}()

	for {
		btcd, err := BtcdConnect(cafile)
		if err != nil {
			log.Info("Retrying btcd connection in 5 seconds")
			time.Sleep(5 * time.Second)
			continue
		}
		updateBtcd <- btcd

		NotifyBtcdConnection(frontendNotificationMaster)
		log.Info("Established connection to btcd")

		// Perform handshake.
		if err := Handshake(btcd); err != nil {
			var message string
			if jsonErr, ok := err.(*btcjson.Error); ok {
				message = jsonErr.Message
			} else {
				message = err.Error()
			}
			log.Errorf("Cannot complete handshake: %v", message)
			log.Info("Retrying btcd connection in 5 seconds")
			time.Sleep(5 * time.Second)
			continue
		}

		// Block goroutine until the connection is lost.
		<-btcd.closed
		NotifyBtcdConnection(frontendNotificationMaster)
		log.Info("Lost btcd connection")
	}
}

// OpenAccounts attempts to open all saved accounts.
func OpenAccounts() {
	// If the network (account) directory is missing, but the temporary
	// directory exists, move it.  This is unlikely to happen, but possible,
	// if writing out every account file at once to a tmp directory (as is
	// done for changing a wallet passphrase) and btcwallet closes after
	// removing the network directory but before renaming the temporary
	// directory.
	netDir := networkDir(cfg.Net())
	tmpNetDir := tmpNetworkDir(cfg.Net())
	if !fileExists(netDir) && fileExists(tmpNetDir) {
		if err := Rename(tmpNetDir, netDir); err != nil {
			log.Errorf("Cannot move temporary network dir: %v", err)
			return
		}
	}

	// The default account must exist, or btcwallet acts as if no
	// wallets/accounts have been created yet.
	if err := accountstore.OpenAccount("", cfg); err != nil {
		switch err.(type) {
		case *WalletOpenError:
			log.Errorf("Default account wallet file unreadable: %v", err)
			return

		default:
			log.Warnf("Non-critical problem opening an account file: %v", err)
		}
	}

	// Read all filenames in the account directory, and look for any
	// filenames matching '*-wallet.bin'.  These are wallets for
	// additional saved accounts.
	accountDir, err := os.Open(netDir)
	if err != nil {
		// Can't continue.
		log.Errorf("Unable to open account directory: %v", err)
		return
	}
	defer accountDir.Close()
	fileNames, err := accountDir.Readdirnames(0)
	if err != nil {
		// fileNames might be partially set, so log an error and
		// at least try to open some accounts.
		log.Errorf("Unable to read all account files: %v", err)
	}
	var accounts []string
	for _, file := range fileNames {
		if strings.HasSuffix(file, "-wallet.bin") {
			name := strings.TrimSuffix(file, "-wallet.bin")
			accounts = append(accounts, name)
		}
	}

	// Open all additional accounts.
	for _, a := range accounts {
		// Log txstore/utxostore errors as these will be recovered
		// from with a rescan, but wallet errors must be returned
		// to the caller.
		if err := accountstore.OpenAccount(a, cfg); err != nil {
			switch err.(type) {
			case *WalletOpenError:
				log.Errorf("Error opening account's wallet: %v", err)

			default:
				log.Warnf("Non-critical error opening an account file: %v", err)
			}
		}
	}
}

var accessRPC = make(chan *AccessCurrentRPCConn)

// AccessCurrentRPCConn is used to access the current RPC connection
// from the goroutine managing btcd-side RPC connections.
type AccessCurrentRPCConn struct {
	rpc chan RPCConn
}

// CurrentRPCConn returns the most recently-connected btcd-side
// RPC connection.
func CurrentRPCConn() RPCConn {
	access := &AccessCurrentRPCConn{
		rpc: make(chan RPCConn),
	}
	accessRPC <- access
	return <-access.rpc
}
