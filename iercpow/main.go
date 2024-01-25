package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
)

var (
	ADDR_ZERO              = common.HexToAddress("0x0000000000000000000000000000000000000000")
	LEN_FOR_THREADS uint64 = 100000
	GlobalCount     atomic.Uint64
	LastCount       uint64 = 0
	BlockHigh       atomic.Value
)

type MatchedTx struct {
	WalletAddr common.Address     `json:"wallet_addr"`
	Tx         *types.Transaction `json:"tx"`
	RawData    string             `json:"raw_tx"`
}

type MintConfig struct {
	RPCNode     string   `json:"rpc"`
	SendTx      bool     `json:"sendTx"`
	Count       int      `json:"count"`
	Threads     int      `json:"threads"`
	P           string   `json:"p"`
	HashPre     string   `json:"hashPre"`
	Tick        string   `json:"tick"`
	Amt         string   `json:"amt"`
	PrivateKeys []string `json:"private_keys"`
}

type Worker struct {
	index        int
	m            MintConfig
	findHashChan chan *types.Transaction
	nonce        uint64
	curNonce     *atomic.Uint64
	start        uint64
	threads      uint64
	w            *Wallet
	cancel       context.Context
}

type MatchCount struct {
	count int
	nonce uint64
}

func newWorker(index int, findHashChan chan *types.Transaction, imNonce uint64, nonce *atomic.Uint64, start uint64, threads uint64, mintData *MintConfig, w *Wallet, cancel context.Context) *Worker {
	return &Worker{
		index:        index,
		findHashChan: findHashChan,
		nonce:        imNonce,
		curNonce:     nonce,
		start:        start,
		threads:      threads,
		m:            *mintData,
		w:            w,
		cancel:       cancel,
	}
}

func (w *Worker) startMine() {
	fmt.Println("worker", w.index, "start mine: nonce", w.nonce)

	value := big.NewInt(0)

	// float gas
	gwei := big.NewInt(1000000000)                                             // 1 Gwei
	tip := new(big.Float).Mul(big.NewFloat(0.21), new(big.Float).SetInt(gwei)) // 0.21 Gwei
	gasTipCap, _ := tip.Int(nil)                                               // 将浮点数转换为整数

	innerTx := &types.DynamicFeeTx{
		ChainID:   w.w.ChainID,
		Nonce:     w.nonce,
		GasTipCap: gasTipCap,                                                // maxPriorityFeePerGas 10
		GasFeeCap: new(big.Int).Mul(big.NewInt(1000000000), big.NewInt(70)), // max Fee 100
		Gas:       23100,
		To:        &ADDR_ZERO,
		Value:     value,
	}
	fixedStr := fmt.Sprintf("data:application/json,{\"p\":\"%s\",\"op\":\"mint\",\"tick\":\"%s\",\"use_point\":\"12000\",", w.m.P, w.m.Tick)
	var t uint64 = 0
	for ; ; t++ {
		start := w.start + w.threads*t*LEN_FOR_THREADS
		end := start + LEN_FOR_THREADS
		for nonce := start; nonce < end; nonce++ {
			select {
			case <-w.cancel.Done():
				fmt.Println("work", w.index, "exited")
				return
			default:
			}
			GlobalCount.Add(1)
			inputStr := fmt.Sprintf("%s\"block\":\"%s\",\"nonce\":\"%d\"}", fixedStr, BlockHigh.Load().(string), nonce)
			//fmt.Println(inputStr)
			innerTx.Data = []byte(inputStr)
			tx := types.NewTx(innerTx)
			signTx, err := w.w.SignTx(tx)
			if err != nil {
				panic(err)
			}
			if strings.HasPrefix(signTx.Hash().String(), w.m.HashPre) {
				fmt.Println("find matched hash", signTx.Hash().Hex(), "exit", w.index)
				w.findHashChan <- signTx
				return
			}

			if w.nonce != w.curNonce.Load() {
				fmt.Println("exit find hash", w.index)
				return
			}
		}
	}
}

func HashRateStatistic() {
	interval := 5 * time.Second
	timer := time.NewTicker(interval)

	go func() {
		for {
			select {
			case <-timer.C:
				count := GlobalCount.Load() - LastCount
				LastCount = GlobalCount.Load()
				fmt.Printf("Hash count %d, Hashrate: %dH/s\n", count, count/5)
			}
		}
	}()
}

func BlockHighStatistic(client *ethclient.Client) {
	header, err := client.HeaderByNumber(context.Background(), nil)
	if err != nil {
		log.Fatalln("获取区块高度失败", err)
	}
	BlockHigh.Store((header.Number.Add(header.Number, big.NewInt(2))).String())
	tick := time.NewTicker(14 * time.Second)
	go func() {
		for {
			select {
			case <-tick.C:
				header, err = client.HeaderByNumber(context.Background(), nil)
				if err != nil {
					fmt.Println("获取区块高度失败", err)
				} else {
					fmt.Println("当前区块高度:", header.Number.String())
					BlockHigh.Store(header.Number.String())
				}
			}
		}
	}()
}

func initCfg() *MintConfig {
	var mintData MintConfig
	content, _ := os.ReadFile("config.json")
	err := json.Unmarshal(content, &mintData)
	if err != nil {
		fmt.Println("Read config json error")
		return nil
	}
	return &mintData
}

func initMatchedTx() []*MatchedTx {
	matchedTxs := make([]*MatchedTx, 0)
	content, err := os.ReadFile("matchtx.json")
	if err != nil {
		fmt.Println("Read matchtx json error, ignore")
	} else {
		err = json.Unmarshal(content, &matchedTxs)
		if err != nil {
			fmt.Println("Read config json error")
		}
	}
	return matchedTxs
}

func main() {
	var chainId int64 = 1
	mintConfig := initCfg()
	if mintConfig == nil {
		fmt.Println("get config error, pls set config.json")
		return
	}

	client, err := ethclient.Dial(mintConfig.RPCNode)
	if err != nil {
		fmt.Println("Connect rpc error")
		return
	}
	// 检查已经存好的，避免重跑
	findTxs := make([]MatchedTx, 0)
	matchedCount := make(map[string]*MatchCount, 0)
	matchTxs := initMatchedTx()
	for _, mtx := range matchTxs {
		addr := mtx.WalletAddr.String()
		mc, ok := matchedCount[addr]
		if !ok {
			mc = &MatchCount{
				count: 0,
				nonce: mtx.Tx.Nonce(),
			}
			matchedCount[addr] = mc
		} else {
			mc.count++
			mc.nonce = mtx.Tx.Nonce()
		}

		matchData := MatchedTx{
			WalletAddr: mtx.WalletAddr,
			Tx:         mtx.Tx,
			RawData:    mtx.RawData,
		}
		findTxs = append(findTxs, matchData)

		fmt.Println("Addr", addr, "already find", mc.count, "nonce", mc.nonce)
	}

	wallets := make([]*Wallet, 0)
	for idx, pkey := range mintConfig.PrivateKeys {
		w, err := NewWallet(pkey, client, chainId)
		if err != nil {
			fmt.Println("Create wallet error with index", idx, "pkey", pkey, "err", err)
			continue
		}
		wallets = append(wallets, w)
		fmt.Println("Add wallet:", w.Address)
	}
	HashRateStatistic()
	BlockHighStatistic(client)
	for i := 0; i < mintConfig.Count; i++ {
		timestamp := time.Now().UnixMilli()
		onConfirmTxChan := make(chan MatchedTx)
		workerNum := 0
		for _, w := range wallets {
			nonce := uint64(0)
			if mc, ok := matchedCount[w.Address.String()]; ok {
				if mc.count >= i {
					w.Nonce.Store(mc.nonce + 1)
					continue
				}
			}
			nonce = w.Nonce.Load()
			go Term(nonce, mintConfig.Threads, timestamp, mintConfig, w, client, onConfirmTxChan)
			workerNum++
		}
		for i := 0; i < workerNum; i++ {
			findTxs = append(findTxs, <-onConfirmTxChan)
			outfile, _ := os.Create("matchtx.json")
			jsonData, err := json.MarshalIndent(findTxs, "", "  ")
			if err != nil {
				fmt.Println("Marshal json error", err)
			}
			_, _ = outfile.Write(jsonData)
			_ = outfile.Close()
		}
	}
}

func Term(nonce uint64, workers int, timestamp int64, mintConfig *MintConfig, w *Wallet, client *ethclient.Client, sendTx chan MatchedTx) {
	var WNonce atomic.Uint64
	WNonce.Store(nonce)
	onHashFindChn := make(chan *types.Transaction)
	ctx, cancelFunc := context.WithCancel(context.Background())
	imNonce := WNonce.Load()
	for t := 0; t < workers; t++ {
		start := uint64(timestamp) + uint64(t)*LEN_FOR_THREADS
		worker := newWorker(t, onHashFindChn, imNonce, &WNonce, start, uint64(workers), mintConfig, w, ctx)
		go worker.startMine()
	}
	select {
	case tx := <-onHashFindChn:
		w.Nonce.Store(WNonce.Add(1))
		rawTx, _ := w.GetRawTx(tx)
		if mintConfig.SendTx {
			err := client.SendTransaction(context.Background(), tx)
			if err != nil {
				fmt.Println("send transaction failed:", err)
			}
		}
		matchData := MatchedTx{
			WalletAddr: w.Address,
			Tx:         tx,
			RawData:    "0x" + common.Bytes2Hex(rawTx),
		}
		sendTx <- matchData
		fmt.Println("find tx hash:", tx.Hash().Hex())
		cancelFunc()
	}
}
