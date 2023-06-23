package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/deroproject/derohe/cryptography/crypto"
	"github.com/deroproject/derohe/rpc"

	"github.com/docopt/docopt-go"
	"github.com/ybbus/jsonrpc"

	"github.com/civilware/Gnomon/indexer"
	"github.com/civilware/Gnomon/storage"
	"github.com/civilware/Gnomon/structures"
)

var walletRPCClient jsonrpc.RPCClient
var derodRPCClient jsonrpc.RPCClient
var scid string
var ringsize uint64
var prevTH int64
var pollTime time.Duration
var thAddition int64
var gnomonIndexes []*structures.GnomonSCIDQuery
var mux sync.Mutex
var version = "0.1.2"

var command_line string = `Gnomon
Gnomon SC Index Registration Service: As the Gnomon SCID owner, you can automatically poll your local gnomon instance for new SCIDs to append to the index SC

Usage:
  gnomonsc [options]
  gnomonsc -h | --help

Options:
  -h --help     Show this screen.
  --daemon-rpc-address=<127.0.0.1:40402>	Connect to daemon rpc.
  --wallet-rpc-address=<127.0.0.1:40403>	Connect to wallet rpc.
  --gnomon-api-address=<127.0.0.1:8082>	Gnomon api to connect to.
  --block-deploy-buffer=<10>	Block buffer inbetween SC calls. This is for safety, will be hardcoded to minimum of 2 but can define here any amount (10 default).
  --search-filter=<"Function InputStr(input String, varname String) Uint64">	Defines a search filter to match on installed SCs to add to validated list and index all actions, this will most likely change in the future but can allow for some small variability. Include escapes etc. if required. If nothing is defined, it will pull all (minus hardcoded sc).`

// TODO: Add as a passable param perhaps? Or other. Using ;;; for now, can be anything really.. just think what isn't used in norm SC code iterations
const sf_separator = ";;;"

func main() {
	var err error

	//n := runtime.NumCPU()
	//runtime.GOMAXPROCS(n)

	pollTime, _ = time.ParseDuration("5s")
	ringsize = uint64(2)

	// Inspect argument(s)
	arguments, err := docopt.ParseArgs(command_line, nil, version)

	if err != nil {
		log.Fatalf("[Main] Error while parsing arguments err: %s\n", err)
	}

	// Set variables from arguments
	daemon_rpc_endpoint := "127.0.0.1:40402"
	if arguments["--daemon-rpc-address"] != nil {
		daemon_rpc_endpoint = arguments["--daemon-rpc-address"].(string)
	}

	log.Printf("[Main] Using daemon RPC endpoint %s\n", daemon_rpc_endpoint)

	gnomon_api_endpoint := "127.0.0.1:8082"
	if arguments["--gnomon-api-address"] != nil {
		gnomon_api_endpoint = arguments["--gnomon-api-address"].(string)
	}

	log.Printf("[Main] Using gnomon API endpoint %s\n", gnomon_api_endpoint)

	wallet_rpc_endpoint := "127.0.0.1:40403"
	if arguments["--wallet-rpc-address"] != nil {
		wallet_rpc_endpoint = arguments["--wallet-rpc-address"].(string)
	}

	log.Printf("[Main] Using wallet RPC endpoint %s\n", wallet_rpc_endpoint)

	thAddition = int64(10)
	if arguments["--block-deploy-buffer"] != nil {
		thAddition, err = strconv.ParseInt(arguments["--block-deploy-buffer"].(string), 10, 64)
		if err != nil {
			log.Fatalf("[Main] ERROR while converting --block-deploy-buffer to int64\n")
			return
		}
		if thAddition < 2 {
			thAddition = int64(2)
		}
	}

	var search_filter []string
	if arguments["--search-filter"] != nil {
		search_filter_nonarr := arguments["--search-filter"].(string)
		search_filter = strings.Split(search_filter_nonarr, sf_separator)
		log.Printf("[Main] Using search filter: %v\n", search_filter)
	} else {
		log.Printf("[Main] No search filter defined.. grabbing all.\n")
	}

	log.Printf("[Main] Using block deploy buffer of '%v' blocks.\n", thAddition)

	// wallet/derod rpc clients
	walletRPCClient = jsonrpc.NewClient("http://" + wallet_rpc_endpoint + "/json_rpc")
	derodRPCClient = jsonrpc.NewClient("http://" + daemon_rpc_endpoint + "/json_rpc")

	// Get testnet/mainnet
	var info rpc.GetInfo_Result
	err = derodRPCClient.CallFor(&info, "get_info")
	if err != nil {
		log.Printf("ERR: %v", err)
		return
	}

	// SCID
	switch info.Testnet {
	case false:
		scid = "a05395bb0cf77adc850928b0db00eb5ca7a9ccbafd9a38d021c8d299ad5ce1a4"
	case true:
		scid = "c9d23d2fc3aaa8e54e238a2218c0e5176a6e48780920fd8474fac5b0576110a2"
	}

	for {
		fetchGnomonIndexes(gnomon_api_endpoint)
		runGnomonIndexer(daemon_rpc_endpoint, gnomon_api_endpoint, search_filter)
		log.Printf("[Main] Round completed. Sleeping 1 minute for next round.")
		time.Sleep(60 * time.Second)
	}
}

func fetchGnomonIndexes(gnomonendpoint string) {
	mux.Lock()
	defer mux.Unlock()
	var lastQuery map[string]interface{}
	var err error
	log.Printf("[fetchGnomonIndexes] Getting sc data")
	rs, err := http.Get("http://" + gnomonendpoint + "/api/indexedscs")
	if err != nil {
		log.Printf("[fetchGnomonIndexes] gnomon query err %s\n", err)
	} else {
		log.Printf("[fetchGnomonIndexes] Retrieved sc data... reading in and building structures.")
		b, err := io.ReadAll(rs.Body)
		if err != nil {
			log.Printf("[fetchGnomonIndexes] error reading body %s\n", err)
		} else {
			err = json.Unmarshal(b, &lastQuery)
			if err != nil {
				log.Printf("[fetchGnomonIndexes] error unmarshalling b %s\n", err)
			}

			if lastQuery["indexdetails"] != nil {
				var changes []*structures.GnomonSCIDQuery
				for _, v := range lastQuery["indexdetails"].([]interface{}) {
					x := v.(map[string]interface{})
					//log.Printf("inputscid(\"%v\", \"%v\", %v)", x["SCID"], x["Owner"], x["Height"])
					height := x["Height"].(float64)
					changes = append(changes, &structures.GnomonSCIDQuery{Owner: x["Owner"].(string), Height: uint64(height), SCID: x["SCID"].(string)})
				}
				gnomonIndexes = changes
			}
		}
	}
}

func runGnomonIndexer(derodendpoint string, gnomonendpoint string, search_filter []string) {
	mux.Lock()
	defer mux.Unlock()
	var lastQuery map[string]interface{}
	var currheight int64
	log.Printf("[runGnomonIndexer] Provisioning new RAM indexer...")
	graviton_backend, err := storage.NewGravDBRAM("25ms")
	if err != nil {
		log.Printf("[runGnomonIndexer] Error creating new gravdb: %v", err)
		return
	}

	// Get current height from getinfo api to poll current network states. Fallback to slow and steady mode.
	var defaultIndexer *indexer.Indexer
	log.Printf("[fetchGnomonIndexes] Getting current height data")
	rs, err := http.Get("http://" + gnomonendpoint + "/api/getinfo")
	if err != nil {
		log.Printf("[fetchGnomonIndexes] gnomon height query err %s\n", err)
	} else {
		log.Printf("[fetchGnomonIndexes] Retrieved getinfo data... reading in current height.")
		b, err := io.ReadAll(rs.Body)
		if err != nil {
			log.Printf("[fetchGnomonIndexes] error reading getinfo body %s\n", err)
		} else {
			err = json.Unmarshal(b, &lastQuery)
			if err != nil {
				log.Printf("[fetchGnomonIndexes] error unmarshalling b %s\n", err)
			}

			if lastQuery["getinfo"] != nil {
				for k, v := range lastQuery["getinfo"].(map[string]interface{}) {
					if k == "height" {
						currheight = int64(v.(float64))
					}
				}
			}
		}
	}

	// If we can gather the current height from /api/getinfo then start-topoheight will be passed and fastsync not used. This saves time to not check all SCIDs from gnomon SC. Otherwise default back to "slow and steady" method.
	if currheight > 0 {
		defaultIndexer = indexer.NewIndexer(graviton_backend, nil, "gravdb", nil, currheight, derodendpoint, "daemon", false, false, false)
		defaultIndexer.StartDaemonMode(1)
	} else {
		defaultIndexer = indexer.NewIndexer(graviton_backend, nil, "gravdb", nil, int64(1), derodendpoint, "daemon", false, false, true)
		defaultIndexer.StartDaemonMode(1)
	}

	for {
		if defaultIndexer.ChainHeight <= 1 || defaultIndexer.LastIndexedHeight < defaultIndexer.ChainHeight {
			log.Printf("[runGnomonIndexer] Waiting on defaultIndexer... (%v / %v)", defaultIndexer.LastIndexedHeight, defaultIndexer.ChainHeight)
			time.Sleep(5 * time.Second)
		} else {
			break
		}
	}

	var changes bool
	var variables []*structures.SCIDVariable
	variables, _, _, _ = defaultIndexer.RPC.GetSCVariables(scid, defaultIndexer.ChainHeight, nil, nil, nil)

	log.Printf("[runGnomonIndexer] Looping through discovered SCs and checking to see if any are not indexed.")
	var perc float64
	var tperc, intperc int64
	percStep := 1
	for k, v := range gnomonIndexes {
		// Crude percentage output tracker for longer running operations. Remove later, just debugging purposes.
		perc = (float64(k) / float64(len(gnomonIndexes))) * float64(100)
		intperc = int64(math.Trunc(perc))
		if intperc%int64(percStep) == 0 && tperc < intperc {
			tperc = intperc
			log.Printf("[runGnomonIndexer] Looping... %.0f %% - %v / %v", perc, k, len(gnomonIndexes))
		}

		var contains bool
		var code string
		i := 0
		// This is slower due to lookup each time, however we need to ensure that every instance is checked as blocks happen and other gnomonsc instances could be indexing
		// TODO: Need to track mempool for changes as well
		valuesstringbykey, valuesuint64bykey, err := defaultIndexer.GetSCIDValuesByKey(variables, scid, v.SCID+"height", defaultIndexer.ChainHeight)
		if err != nil {
			// Do not attempt to index if err is returned. Possible reasons being daemon connectivity failure etc.
			log.Printf("[runGnomonIndexer] Skipping index of '%v' this round. GetSCIDValuesByKey errored out - %v", scid, err)
			continue
		}
		if len(valuesstringbykey) > 0 {
			i++
		}
		if len(valuesuint64bykey) > 0 {
			i++
		}

		if i == 0 {
			// If we can get the SC and searchfilter is "" (get all), contains is true. Otherwise evaluate code against searchfilter
			if len(search_filter) == 0 {
				contains = true
			} else {
				_, code, _, _ = defaultIndexer.RPC.GetSCVariables(v.SCID, defaultIndexer.ChainHeight, nil, nil, nil)
				// Ensure scCode is not blank (e.g. an invalid scid)
				if code != "" {
					for _, sfv := range search_filter {
						contains = strings.Contains(code, sfv)
						if contains {
							// Break b/c we want to ensure contains remains true. Only care if it matches at least 1 case
							break
						}
					}
				}
			}

			if contains {
				changes = true
				log.Printf("[runGnomonIndexer] SCID has not been indexed - %v ... Indexing now", v.SCID)
				// Do indexing job here.

				// Check txpool to see if current txns exist for indexing of same SCID
				var txpool []string
				txpool, err = defaultIndexer.RPC.GetTxPool()
				if err != nil {
					log.Printf("[runGnomonIndexer-GetTxPool] ERROR Getting TX Pool - %v . Skipping index of SCID '%v' for safety.\n", err, v.SCID)
					continue
				} else {
					log.Printf("[runGnomonIndexer-GetTxPool] TX Pool List - %v\n", txpool)
				}

				var chashtxns []crypto.Hash
				var inputsc bool
				for _, tx := range txpool {
					var thash crypto.Hash
					copy(thash[:], []byte(tx)[:])
					chashtxns = append(chashtxns, thash)
				}

				cIndex := &structures.BlockTxns{Topoheight: defaultIndexer.ChainHeight, Tx_hashes: chashtxns}
				bl_sctxs, _, _, _, err := defaultIndexer.IndexTxn(cIndex, true)
				if err != nil {
					log.Printf("[runGnomonIndexer-IndexTxn] ERROR - %v . Skipping index of SCID '%v' for safety.\n", err, v.SCID)
					continue
				}

				// If no sc txns, then go ahead and input as nothing is in mempool
				if len(bl_sctxs) == 0 {
					inputsc = true
				} else {
					var txc int
					for _, txpv := range bl_sctxs {
						// Check if any of the mempool txns are for the gnomon SCID
						if txpv.Scid == scid {
							// Mempool txn is pending for gnomon SCID. Check payload to see if the SCID matches the intended SCID to be indexed
							argscid := fmt.Sprintf("%v", txpv.Sc_args.Value("scid", "S"))
							if argscid == v.SCID {
								log.Printf("[runGnomonIndexer-inputscid] Skipping index of SCID '%v' as mempool txn '%v' includes SCID, safety.\n", v.SCID, txpv.Txid)
								txc++
							} else {
								log.Printf("[runGnomonIndexer-inputscid] Gnomon SCID found in mempool txn '%v' . SCID '%v' not in the payload, continuing.\n", txpv.Txid, v.SCID)
							}
						}
					}
					// If no flags raised on a mempool txn matching gnomon scid -> v.SCID . Go ahead and input scid index.
					if txc == 0 {
						inputsc = true
					}
				}

				if inputsc {
					log.Printf("[runGnomonIndexer-inputscid] Clear to input scid '%v'\n", v.SCID)
					// TODO: Support for authenticator/user:password rpc login for wallet interactions
					inputscid(v.SCID, v.Owner, v.Height)
				}
			}
		}
	}
	if !changes {
		log.Printf("[runGnomonIndexer] No changes made.")
	}

	log.Printf("[runGnomonIndexer] Closing temporary indexer...")
	defaultIndexer.Close()
	time.Sleep(5 * time.Second)
	log.Printf("[runGnomonIndexer] Indexer closed.")
}

func inputscid(inpscid string, scowner string, deployheight uint64) {
	// Get gas estimate based on updatecode function to calculate appropriate storage fees to append
	var rpcArgs = rpc.Arguments{}
	rpcArgs = append(rpcArgs, rpc.Argument{Name: "entrypoint", DataType: "S", Value: "InputSCID"})
	rpcArgs = append(rpcArgs, rpc.Argument{Name: "scid", DataType: "S", Value: inpscid})
	rpcArgs = append(rpcArgs, rpc.Argument{Name: "scowner", DataType: "S", Value: scowner})
	rpcArgs = append(rpcArgs, rpc.Argument{Name: "deployheight", DataType: "U", Value: deployheight})
	var transfers []rpc.Transfer

	sendtx(rpcArgs, transfers)
}

func sendtx(rpcArgs rpc.Arguments, transfers []rpc.Transfer) {
	var err error
	var gasstr rpc.GasEstimate_Result
	var addr rpc.GetAddress_Result
	err = walletRPCClient.CallFor(&addr, "GetAddress")
	if addr.Address == "" {
		log.Printf("[GetAddress] Failed - %v", err)
	}
	gasRpc := rpcArgs
	gasRpc = append(gasRpc, rpc.Argument{Name: "SC_ACTION", DataType: "U", Value: rpc.SC_CALL})
	gasRpc = append(gasRpc, rpc.Argument{Name: "SC_ID", DataType: "H", Value: string([]byte(scid))})

	var gasestimateparams rpc.GasEstimate_Params
	if len(transfers) > 0 {
		if ringsize > 2 {
			gasestimateparams = rpc.GasEstimate_Params{SC_RPC: gasRpc, Ringsize: ringsize, Signer: "", Transfers: transfers}
		} else {
			gasestimateparams = rpc.GasEstimate_Params{SC_RPC: gasRpc, Ringsize: ringsize, Signer: addr.Address, Transfers: transfers}
		}
	} else {
		if ringsize > 2 {
			gasestimateparams = rpc.GasEstimate_Params{SC_RPC: gasRpc, Ringsize: ringsize, Signer: ""}
		} else {
			gasestimateparams = rpc.GasEstimate_Params{SC_RPC: gasRpc, Ringsize: ringsize, Signer: addr.Address}
		}
	}
	err = derodRPCClient.CallFor(&gasstr, "DERO.GetGasEstimate", gasestimateparams)
	if err != nil {
		log.Printf("[getGasEstimate] gas estimate err %s\n", err)
		return
	} else {
		log.Printf("[getGasEstimate] gas estimate results: %v", gasstr)
	}
	var txnp rpc.Transfer_Params
	var str rpc.Transfer_Result

	txnp.SC_RPC = gasRpc
	if len(transfers) > 0 {
		txnp.Transfers = transfers
	}
	txnp.Ringsize = ringsize
	txnp.Fees = gasstr.GasStorage

	// Loop through to ensure we haven't recently sent in this session too quickly
	if prevTH != 0 {
		for {
			var info rpc.GetInfo_Result
			err := derodRPCClient.CallFor(&info, "get_info")
			if err != nil {
				log.Printf("ERR: %v", err)
				return
			}

			targetTH := prevTH + thAddition

			if targetTH <= info.TopoHeight {
				prevTH = info.TopoHeight
				break
			} else {
				log.Printf("[sendTX] Waiting until topoheights line up to send next TX [last: %v / curr: %v]", info.TopoHeight, targetTH)
				time.Sleep(pollTime)
			}
		}
	} else {
		var info rpc.GetInfo_Result
		err := derodRPCClient.CallFor(&info, "get_info")
		if err != nil {
			log.Printf("ERR: %v", err)
			return
		}

		prevTH = info.TopoHeight
	}

	// Call Transfer (not scinvoke) since we append fees above like a normal txn.

	err = walletRPCClient.CallFor(&str, "Transfer", txnp)
	if err != nil {
		log.Printf("[sendTx] err: %v", err)
		return
	} else {
		log.Printf("[sendTx] Tx sent successfully - txid: %v", str.TXID)
	}
}
