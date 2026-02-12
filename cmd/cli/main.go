package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"flag"
	"log"
	"net"
	"time"

	"simchain-go/internal/identity"
	"simchain-go/internal/transport/tcp"
	"simchain-go/internal/types"
)

func main() {
	connectFlag := flag.String("connect", "127.0.0.1:50001", "node address to connect to")
	cmdFlag := flag.String("cmd", "submit", "command: submit")
	dataFlag := flag.String("data", "", "transaction payload (for submit)")
	flag.Parse()

	if *cmdFlag == "submit" && *dataFlag == "" {
		log.Fatal("missing --data for submit")
	}

	// 1. Generate Ephemeral Identity
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		log.Fatal(err)
	}
	nodeID, _ := identity.NodeIDFromPublicKey(pub)
	log.Printf("CLI_ID %s", nodeID)

	// 2. Dial
	conn, err := net.DialTimeout("tcp", *connectFlag, 5*time.Second)
	if err != nil {
		log.Fatalf("connect failed: %v", err)
	}
	defer conn.Close()

	// 3. Handshake
	// Use magic/version from standard config
	peerID, _, _, err := tcp.HandshakeOutbound(
		conn, nodeID, priv, pub,
		"127.0.0.1:0", // Ephemeral listener
		"simchain-go", 1,
		2<<20, 5*time.Second, 5*time.Second,
	)
	if err != nil {
		log.Fatalf("handshake failed: %v", err)
	}
	log.Printf("CONNECTED peer=%s", peerID)

	// 4. Handle Command
	switch *cmdFlag {
	case "submit":
		tx := types.NewTransaction(*dataFlag)
		log.Printf("SUBMIT tx=%s payload=%s", tx.ID, *dataFlag)

		msg := types.Message{
			Type:      types.MsgTx,
			From:      nodeID,
			To:        peerID,
			Timestamp: time.Now().UnixMilli(),
			Payload:   types.TxPayload{Tx: tx},
		}

		if err := tcp.WriteFrame(conn, msg, 2<<20, 5*time.Second); err != nil {
			log.Fatalf("send failed: %v", err)
		}
		log.Printf("SENT")
	default:
		log.Fatalf("unknown cmd: %s", *cmdFlag)
	}
}
