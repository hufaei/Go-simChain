# test v3c.py
import socket
import time
import threading
import sys
import json
import random
import argparse

# Configuration
TARGET_HOST = "127.0.0.1"
TARGET_PORT = 50001 # Default port? Or user specified.
TIMEOUT = 5

def encode_frame(payload_dict):
    """Encodes a JSON payload into network frame (Length-Prefix)."""
    data = json.dumps(payload_dict).encode('utf-8')
    length = len(data)
    # BigEndian uint32
    header = length.to_bytes(4, byteorder='big')
    return header + data

def test_tcp_limit(port, count):
    """
    Attempts to establish 'count' connections from localhost.
    Expects only MaxConnsPerIP (default 2) to succeed.
    """
    print(f"[*] Testing TCP Connection Limit (Target: {count} connections)...")
    conns = []
    success = 0
    fail = 0

    for i in range(count):
        try:
            s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
            s.settimeout(2.0)
            s.connect((TARGET_HOST, port))
            # Send handshake immediately or it might close due to idle?
            # V3-B Handshake is required.
            # But IP limit check happens *before* handshake in acceptLoop.
            # So simple connection is enough to trigger IP check.
            conns.append(s)
            success += 1
            print(f"  [+] Conn {i+1}: Success")
        except Exception as e:
            fail += 1
            print(f"  [-] Conn {i+1}: Failed ({e})")
        
        # Check if previous connections are still alive
        # Some might be closed asynchronously by server
        # We try to read 1 byte from them to see if closed
        # But read blocks.
        time.sleep(0.1)

    print(f"[*] Attempted: {count}, Successful Handshake Init: {success}")
    
    # Now verify which are actually alive
    print("[*] Verifying alive connections...")
    alive = 0
    for i, s in enumerate(conns):
        try:
            # Send a garbage byte, server should close if it didn't already
            # Or just check peer count (hard without API).
            # We just hold them.
            # If server closed them, 'send' might fail or 'recv' return 0.
            # Let's try peek.
            s.settimeout(0.1)
            try:
                data = s.recv(1024, socket.MSG_PEEK)
                if len(data) == 0:
                    print(f"  [-] Conn {i+1} closed by server (EOF)")
                else:
                    print(f"  [+] Conn {i+1} still alive")
                    alive += 1
            except socket.timeout:
                # Still alive (read timeout)
                print(f"  [+] Conn {i+1} still alive (Timeout implies open)")
                alive += 1
            except ConnectionResetError:
                 print(f"  [-] Conn {i+1} reset by server")
        except Exception as e:
             print(f"  [-] Conn {i+1} error: {e}")

    print(f"[*] Result: ~{alive} connections maintained. (Expected: 2)")
    # Cleanup
    for s in conns:
        try:
            s.close()
        except:
            pass

def test_mempool_spam(port, count, rate_limit):
    """
    Sends many transactions to fill mempool.
    Does manual handshake first.
    """
    print(f"[*] Testing Mempool Capacity (Sending {count} txs)...")
    
    # 1. Handshake (Simplified, needs crypto helper or just send garbage to trigger rejection?)
    # The server expects valid handshake.
    # Implementing full V3 Handshake in Python is complex (Ed25519).
    # ALTERNATIVE: Use simchain-cli?
    # Or, we assume user uses 'simchain-cli submit'.
    # This script will focus on TCP level limits which don't need full handshake for IP check,
    # but for Mempool we need valid protocol.
    
    print("[!] ERROR: Mempool Spam test requires full Ed25519 handshake which is complex in this script.")
    print("    Please use the provided Go test 'TestMempool_Capacity' or 'simchain-cli' instead.")
    return

def test_slowloris(port):
    """
    Connects and sends 1 byte every 2 seconds.
    Should be disconnected by ReadTimeout (Default 4s) or IdleTimeout.
    Or RateLimiter?
    """
    print("[*] Testing Slowloris (Header Timeout)...")
    try:
        s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        s.connect((TARGET_HOST, port))
        print("  [+] Connected.")
        
        # Send partial header
        s.send(b'\x00')
        print("  [+] Sent 1 byte. Sleeping 5s (> ReadTimeout 4s)...")
        time.sleep(5)
        
        # Try send again
        try:
            s.send(b'\x00')
            print("  [?] Send succeed. Checking if server closed...")
            s.settimeout(1.0)
            if len(s.recv(1024)) == 0:
                print("  [+] PASS: Server closed connection (EOF).")
            else:
                print("  [-] FAIL: Connection still alive.")
        except (BrokenPipeError, ConnectionResetError):
            print("  [+] PASS: Pipe broken (Server closed connection).")
            
    except Exception as e:
        print(f"  [-] Error: {e}")
    finally:
        s.close()

if __name__ == "__main__":
    parser = argparse.ArgumentParser(description='V3-C Manual Verification Script')
    parser.add_argument('mode', choices=['tcp-limit', 'slowloris'], help='Test mode')
    parser.add_argument('--port', type=int, default=50001, help='Node P2P Port')
    parser.add_argument('--count', type=int, default=10, help='Connection count for limit test')
    
    args = parser.parse_args()
    
    if args.mode == 'tcp-limit':
        test_tcp_limit(args.port, args.count)
    elif args.mode == 'slowloris':
        test_slowloris(args.port)
