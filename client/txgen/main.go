package main

import (
	"bufio"
	"encoding/hex"
	"flag"
	"fmt"
	"harmony-benchmark/blockchain"
	"harmony-benchmark/client"
	"harmony-benchmark/consensus"
	"harmony-benchmark/log"
	"harmony-benchmark/node"
	"harmony-benchmark/p2p"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

var utxoPoolMutex sync.Mutex

// Generates at most "maxNumTxs" number of simulated transactions based on the current UtxoPools of all shards.
// The transactions are generated by going through the existing utxos and
// randomly select a subset of them as the input for each new transaction. The output
// address of the new transaction are randomly selected from [0 - N), where N is the total number of fake addresses.
//
// When crossShard=true, besides the selected utxo input, select another valid utxo as input from the same address in a second shard.
// Similarly, generate another utxo output in that second shard.
//
// NOTE: the genesis block should contain N coinbase transactions which add
//       token (1000) to each address in [0 - N). See node.AddTestingAddresses()
//
// Params:
//     shardId                    - the shardId for current shard
//     dataNodes                  - nodes containing utxopools of all shards
//     maxNumTxs                  - the max number of txs to generate
//     crossShard                 - whether to generate cross shard txs
// Returns:
//     all single-shard txs
//     all cross-shard txs
func generateSimulatedTransactions(shardId int, dataNodes []*node.Node, maxNumTxs int, crossShard bool) ([]*blockchain.Transaction, []*blockchain.Transaction) {
	/*
	  UTXO map structure:
	     address - [
	                txId1 - [
	                        outputIndex1 - value1
	                        outputIndex2 - value2
	                       ]
	                txId2 - [
	                        outputIndex1 - value1
	                        outputIndex2 - value2
	                       ]
	               ]
	*/
	var txs []*blockchain.Transaction
	var crossTxs []*blockchain.Transaction
	txsCount := 0

	utxoPoolMutex.Lock()

UTXOLOOP:
	// Loop over all addresses
	for address, txMap := range dataNodes[shardId].UtxoPool.UtxoMap {
		// Loop over all txIds for the address
		for txIdStr, utxoMap := range txMap {
			// Parse TxId
			id, err := hex.DecodeString(txIdStr)
			if err != nil {
				continue
			}
			txId := [32]byte{}
			copy(txId[:], id[:])

			// Loop over all utxos for the txId
			for index, value := range utxoMap {
				if txsCount >= maxNumTxs {
					break UTXOLOOP
				}
				randNum := rand.Intn(100)

				// 30% sample rate to select UTXO to use for new transactions
				if randNum < 30 {
					if crossShard && randNum < 10 { // 30% cross shard transactions: add another txinput from another shard
						// shard with neighboring Id
						crossShardId := (int(dataNodes[shardId].Consensus.ShardID) + 1) % len(dataNodes)

						crossShardNode := dataNodes[crossShardId]
						crossShardUtxosMap := crossShardNode.UtxoPool.UtxoMap[address]

						// Get the cross shard utxo from another shard
						var crossTxin *blockchain.TXInput
						crossUtxoValue := 0
						// Loop over utxos for the same address from the other shard and use the first utxo as the second cross tx input
						for crossTxIdStr, crossShardUtxos := range crossShardUtxosMap {
							// Parse TxId
							id, err := hex.DecodeString(crossTxIdStr)
							if err != nil {
								continue
							}
							crossTxId := [32]byte{}
							copy(crossTxId[:], id[:])

							for crossShardIndex, crossShardValue := range crossShardUtxos {
								crossUtxoValue = crossShardValue
								crossTxin = &blockchain.TXInput{crossTxId, crossShardIndex, address, uint32(crossShardId)}
								break
							}
							if crossTxin != nil {
								break
							}
						}

						// Add the utxo from current shard
						txin := blockchain.TXInput{txId, index, address, dataNodes[shardId].Consensus.ShardID}
						txInputs := []blockchain.TXInput{txin}

						// Add the utxo from the other shard, if any
						if crossTxin != nil {
							txInputs = append(txInputs, *crossTxin)
						}

						// Spend the utxo from the current shard to a random address in [0 - N)
						txout := blockchain.TXOutput{value, strconv.Itoa(rand.Intn(10000)), dataNodes[shardId].Consensus.ShardID}
						txOutputs := []blockchain.TXOutput{txout}

						// Spend the utxo from the other shard, if any, to a random address in [0 - N)
						if crossTxin != nil {
							crossTxout := blockchain.TXOutput{crossUtxoValue, strconv.Itoa(rand.Intn(10000)), uint32(crossShardId)}
							txOutputs = append(txOutputs, crossTxout)
						}

						// Construct the new transaction
						tx := blockchain.Transaction{[32]byte{}, txInputs, txOutputs, nil}
						tx.SetID()

						crossTxs = append(crossTxs, &tx)
						txsCount++
					} else {
						// Add the utxo as new tx input
						txin := blockchain.TXInput{txId, index, address, dataNodes[shardId].Consensus.ShardID}

						// Spend the utxo to a random address in [0 - N)
						txout := blockchain.TXOutput{value, strconv.Itoa(rand.Intn(10000)), dataNodes[shardId].Consensus.ShardID}
						tx := blockchain.Transaction{[32]byte{}, []blockchain.TXInput{txin}, []blockchain.TXOutput{txout}, nil}
						tx.SetID()

						txs = append(txs, &tx)
						txsCount++
					}
				}
			}
		}
	}
	utxoPoolMutex.Unlock()

	return txs, crossTxs
}

// Gets all the validator peers
func getValidators(config string) []p2p.Peer {
	file, _ := os.Open(config)
	fscanner := bufio.NewScanner(file)
	var peerList []p2p.Peer
	for fscanner.Scan() {
		p := strings.Split(fscanner.Text(), " ")
		ip, port, status := p[0], p[1], p[2]
		if status == "leader" || status == "client" {
			continue
		}
		peer := p2p.Peer{Port: port, Ip: ip}
		peerList = append(peerList, peer)
	}
	return peerList
}

// Gets all the leader peers and corresponding shard Ids
func getLeadersAndShardIds(config *[][]string) ([]p2p.Peer, []uint32) {
	var peerList []p2p.Peer
	var shardIds []uint32
	for _, node := range *config {
		ip, port, status, shardId := node[0], node[1], node[2], node[3]
		if status == "leader" {
			peerList = append(peerList, p2p.Peer{Ip: ip, Port: port})
			val, err := strconv.Atoi(shardId)
			if err == nil {
				shardIds = append(shardIds, uint32(val))
			} else {
				log.Error("[Generator] Error parsing the shard Id ", shardId)
			}
		}
	}
	return peerList, shardIds
}

// Parse the config file and return a 2d array containing the file data
func readConfigFile(configFile string) [][]string {
	file, _ := os.Open(configFile)
	fscanner := bufio.NewScanner(file)

	result := [][]string{}
	for fscanner.Scan() {
		p := strings.Split(fscanner.Text(), " ")
		result = append(result, p)
	}
	return result
}

// Gets the port of the client node in the config
func getClientPort(config *[][]string) string {
	for _, node := range *config {
		_, port, status, _ := node[0], node[1], node[2], node[3]
		if status == "client" {
			return port
		}
	}
	return ""
}

// A utility func that counts the total number of utxos in a pool.
func countNumOfUtxos(utxoPool *blockchain.UTXOPool) int {
	countAll := 0
	for _, utxoMap := range utxoPool.UtxoMap {
		for txIdStr, val := range utxoMap {
			_ = val
			id, err := hex.DecodeString(txIdStr)
			if err != nil {
				continue
			}

			txId := [32]byte{}
			copy(txId[:], id[:])
			for _, utxo := range val {
				_ = utxo
				countAll++
			}
		}
	}
	return countAll
}

func main() {
	configFile := flag.String("config_file", "local_config.txt", "file containing all ip addresses and config")
	maxNumTxsPerBatch := flag.Int("max_num_txs_per_batch", 100000, "number of transactions to send per message")
	logFolder := flag.String("log_folder", "latest", "the folder collecting the logs of this execution")
	flag.Parse()

	// Read the configs
	config := readConfigFile(*configFile)
	leaders, shardIds := getLeadersAndShardIds(&config)

	// Do cross shard tx if there are more than one shard
	crossShard := len(shardIds) > 1

	// TODO(Richard): refactor this chuck to a single method
	// Setup a logger to stdout and log file.
	logFileName := fmt.Sprintf("./%v/txgen.log", *logFolder)
	h := log.MultiHandler(
		log.StdoutHandler,
		log.Must.FileHandler(logFileName, log.LogfmtFormat()), // Log to file
		// log.Must.NetHandler("tcp", ":3000", log.JSONFormat()) // Log to remote
	)
	log.Root().SetHandler(h)

	// Nodes containing utxopools to mirror the shards' data in the network
	nodes := []*node.Node{}
	for _, shardId := range shardIds {
		node := node.New(&consensus.Consensus{ShardID: shardId})
		// Assign many fake addresses so we have enough address to place with at first
		node.AddTestingAddresses(10000)
		nodes = append(nodes, node)
	}

	// Client/txgenerator server node setup
	clientPort := getClientPort(&config)
	consensusObj := consensus.NewConsensus("0", clientPort, "0", nil, p2p.Peer{})
	clientNode := node.New(consensusObj)

	if clientPort != "" {
		clientNode.Client = client.NewClient(&leaders)

		// This func is used to update the client's utxopool when new blocks are received from the leaders
		updateBlocksFunc := func(blocks []*blockchain.Block) {
			log.Debug("Received new block from leader", "len", len(blocks))
			for _, block := range blocks {
				for _, node := range nodes {
					if node.Consensus.ShardID == block.ShardId {
						log.Debug("Adding block from leader", "shardId", block.ShardId)
						// Add it to blockchain
						utxoPoolMutex.Lock()
						node.AddNewBlock(block)
						utxoPoolMutex.Unlock()
					} else {
						continue
					}
				}
			}
		}
		clientNode.Client.UpdateBlocks = updateBlocksFunc

		// Start the client server to listen to leader's message
		go func() {
			clientNode.StartServer(clientPort)
		}()

	}

	// Transaction generation process
	time.Sleep(10 * time.Second) // wait for nodes to be ready
	start := time.Now()
	totalTime := 300.0 //run for 5 minutes

	for true {
		t := time.Now()
		if t.Sub(start).Seconds() >= totalTime {
			log.Debug("Generator timer ended.", "duration", (int(t.Sub(start))), "startTime", start, "totalTime", totalTime)
			break
		}

		allCrossTxs := []*blockchain.Transaction{}
		// Generate simulated transactions
		for i, leader := range leaders {
			txs, crossTxs := generateSimulatedTransactions(i, nodes, *maxNumTxsPerBatch, crossShard)
			allCrossTxs = append(allCrossTxs, crossTxs...)

			log.Debug("[Generator] Sending single-shard txs ...", "leader", leader, "numTxs", len(txs), "numCrossTxs", len(crossTxs))
			msg := node.ConstructTransactionListMessage(txs)
			p2p.SendMessage(leader, msg)
			// Note cross shard txs are later sent in batch
		}

		if len(allCrossTxs) > 0 {
			log.Debug("[Generator] Broadcasting cross-shard txs ...", "allCrossTxs", len(allCrossTxs))
			msg := node.ConstructTransactionListMessage(allCrossTxs)
			p2p.BroadcastMessage(leaders, msg)

			// Put cross shard tx into a pending list waiting for proofs from leaders
			if clientPort != "" {
				clientNode.Client.PendingCrossTxsMutex.Lock()
				for _, tx := range allCrossTxs {
					clientNode.Client.PendingCrossTxs[tx.ID] = tx
				}
				clientNode.Client.PendingCrossTxsMutex.Unlock()
			}
		}

		time.Sleep(500 * time.Millisecond) // Send a batch of transactions periodically
	}

	// Send a stop message to stop the nodes at the end
	msg := node.ConstructStopMessage()
	peers := append(getValidators(*configFile), leaders...)
	p2p.BroadcastMessage(peers, msg)
}
