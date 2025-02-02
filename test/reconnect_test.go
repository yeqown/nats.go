// Copyright 2013-2023 The NATS Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package test

import (
	"fmt"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

func startReconnectServer(t *testing.T) *server.Server {
	return RunServerOnPort(TEST_PORT)
}

func TestReconnectTotalTime(t *testing.T) {
	opts := nats.GetDefaultOptions()
	totalReconnectTime := time.Duration(opts.MaxReconnect) * opts.ReconnectWait
	if totalReconnectTime < (2 * time.Minute) {
		t.Fatalf("Total reconnect time should be at least 2 mins: Currently %v\n",
			totalReconnectTime)
	}
}

func TestDefaultReconnectJitter(t *testing.T) {
	opts := nats.GetDefaultOptions()
	if opts.ReconnectJitter != nats.DefaultReconnectJitter {
		t.Fatalf("Expected default jitter for non TLS to be %v, got %v", nats.DefaultReconnectJitter, opts.ReconnectJitter)
	}
	if opts.ReconnectJitterTLS != nats.DefaultReconnectJitterTLS {
		t.Fatalf("Expected default jitter for TLS to be %v, got %v", nats.DefaultReconnectJitterTLS, opts.ReconnectJitterTLS)
	}
}

func TestReconnectDisallowedFlags(t *testing.T) {
	ts := startReconnectServer(t)
	defer ts.Shutdown()

	ch := make(chan bool)
	opts := nats.GetDefaultOptions()
	opts.Url = fmt.Sprintf("nats://127.0.0.1:%d", TEST_PORT)
	opts.AllowReconnect = false
	opts.ClosedCB = func(_ *nats.Conn) {
		ch <- true
	}
	nc, err := opts.Connect()
	if err != nil {
		t.Fatalf("Should have connected ok: %v", err)
	}
	defer nc.Close()

	ts.Shutdown()

	if e := Wait(ch); e != nil {
		t.Fatal("Did not trigger ClosedCB correctly")
	}
}

func TestReconnectAllowedFlags(t *testing.T) {
	ts := startReconnectServer(t)
	defer ts.Shutdown()
	ch := make(chan bool)
	dch := make(chan bool)
	opts := nats.GetDefaultOptions()
	opts.Url = fmt.Sprintf("nats://127.0.0.1:%d", TEST_PORT)
	opts.AllowReconnect = true
	opts.MaxReconnect = 2
	opts.ReconnectWait = 1 * time.Second
	nats.ReconnectJitter(0, 0)(&opts)

	opts.ClosedCB = func(_ *nats.Conn) {
		ch <- true
	}
	opts.DisconnectedErrCB = func(_ *nats.Conn, _ error) {
		dch <- true
	}
	nc, err := opts.Connect()
	if err != nil {
		t.Fatalf("Should have connected ok: %v", err)
	}
	defer nc.Close()

	ts.Shutdown()

	// We want wait to timeout here, and the connection
	// should not trigger the Close CB.
	if e := WaitTime(ch, 500*time.Millisecond); e == nil {
		t.Fatal("Triggered ClosedCB incorrectly")
	}

	// We should wait to get the disconnected callback to ensure
	// that we are in the process of reconnecting.
	if e := Wait(dch); e != nil {
		t.Fatal("DisconnectedErrCB should have been triggered")
	}

	if !nc.IsReconnecting() {
		t.Fatal("Expected to be in a reconnecting state")
	}

	// clear the CloseCB since ch will block
	nc.Opts.ClosedCB = nil
}

func TestConnCloseBreaksReconnectLoop(t *testing.T) {
	ts := startReconnectServer(t)
	defer ts.Shutdown()

	cch := make(chan bool)

	opts := reconnectOpts
	// Bump the max reconnect attempts
	opts.MaxReconnect = 100
	opts.ClosedCB = func(_ *nats.Conn) {
		cch <- true
	}
	nc, err := opts.Connect()
	if err != nil {
		t.Fatalf("Should have connected ok: %v", err)
	}
	defer nc.Close()
	nc.Flush()

	// Shutdown the server
	ts.Shutdown()

	// Wait a second, then close the connection
	time.Sleep(time.Second)

	// Close the connection, this should break the reconnect loop.
	// Do this in a go routine since the issue was that Close()
	// would block until the reconnect loop is done.
	go nc.Close()

	// Even on Windows (where a createConn takes more than a second)
	// we should be able to break the reconnect loop with the following
	// timeout.
	if err := WaitTime(cch, 3*time.Second); err != nil {
		t.Fatal("Did not get a closed callback")
	}
}

func TestBasicReconnectFunctionality(t *testing.T) {
	ts := startReconnectServer(t)
	defer ts.Shutdown()

	ch := make(chan bool)
	dch := make(chan bool, 2)

	opts := reconnectOpts

	opts.DisconnectedErrCB = func(_ *nats.Conn, _ error) {
		dch <- true
	}

	nc, err := opts.Connect()
	if err != nil {
		t.Fatalf("Should have connected ok: %v\n", err)
	}
	defer nc.Close()
	ec, err := nats.NewEncodedConn(nc, nats.DEFAULT_ENCODER)
	if err != nil {
		t.Fatalf("Failed to create an encoded connection: %v\n", err)
	}

	testString := "bar"
	ec.Subscribe("foo", func(s string) {
		if s != testString {
			t.Fatal("String doesn't match")
		}
		ch <- true
	})
	ec.Flush()

	ts.Shutdown()
	// server is stopped here...

	if err := Wait(dch); err != nil {
		t.Fatalf("Did not get the disconnected callback on time\n")
	}

	if err := ec.Publish("foo", testString); err != nil {
		t.Fatalf("Failed to publish message: %v\n", err)
	}

	ts = startReconnectServer(t)
	defer ts.Shutdown()

	if err := ec.FlushTimeout(5 * time.Second); err != nil {
		t.Fatalf("Error on Flush: %v", err)
	}

	if e := Wait(ch); e != nil {
		t.Fatal("Did not receive our message")
	}

	expectedReconnectCount := uint64(1)
	reconnectCount := ec.Conn.Stats().Reconnects

	if reconnectCount != expectedReconnectCount {
		t.Fatalf("Reconnect count incorrect: %d vs %d\n",
			reconnectCount, expectedReconnectCount)
	}
}

func TestExtendedReconnectFunctionality(t *testing.T) {
	ts := startReconnectServer(t)
	defer ts.Shutdown()

	opts := reconnectOpts
	dch := make(chan bool, 2)
	opts.DisconnectedErrCB = func(_ *nats.Conn, _ error) {
		dch <- true
	}
	rch := make(chan bool, 1)
	opts.ReconnectedCB = func(_ *nats.Conn) {
		rch <- true
	}
	nc, err := opts.Connect()
	if err != nil {
		t.Fatalf("Should have connected ok: %v", err)
	}
	defer nc.Close()
	ec, err := nats.NewEncodedConn(nc, nats.DEFAULT_ENCODER)
	if err != nil {
		t.Fatalf("Failed to create an encoded connection: %v\n", err)
	}
	testString := "bar"
	received := int32(0)

	ec.Subscribe("foo", func(s string) {
		atomic.AddInt32(&received, 1)
	})

	sub, _ := ec.Subscribe("foobar", func(s string) {
		atomic.AddInt32(&received, 1)
	})

	ec.Publish("foo", testString)
	ec.Flush()

	ts.Shutdown()
	// server is stopped here..

	// wait for disconnect
	if e := WaitTime(dch, 2*time.Second); e != nil {
		t.Fatal("Did not receive a disconnect callback message")
	}

	// Sub while disconnected
	ec.Subscribe("bar", func(s string) {
		atomic.AddInt32(&received, 1)
	})

	// Unsub foobar while disconnected
	sub.Unsubscribe()

	if err = ec.Publish("foo", testString); err != nil {
		t.Fatalf("Received an error after disconnect: %v\n", err)
	}

	if err = ec.Publish("bar", testString); err != nil {
		t.Fatalf("Received an error after disconnect: %v\n", err)
	}

	ts = startReconnectServer(t)
	defer ts.Shutdown()

	// server is restarted here..
	// wait for reconnect
	if e := WaitTime(rch, 2*time.Second); e != nil {
		t.Fatal("Did not receive a reconnect callback message")
	}

	if err = ec.Publish("foobar", testString); err != nil {
		t.Fatalf("Received an error after server restarted: %v\n", err)
	}

	if err = ec.Publish("foo", testString); err != nil {
		t.Fatalf("Received an error after server restarted: %v\n", err)
	}

	ch := make(chan bool)
	ec.Subscribe("done", func(b bool) {
		ch <- true
	})
	ec.Publish("done", true)

	if e := Wait(ch); e != nil {
		t.Fatal("Did not receive our message")
	}

	// Sleep a bit to guarantee scheduler runs and process all subs.
	time.Sleep(50 * time.Millisecond)

	if atomic.LoadInt32(&received) != 4 {
		t.Fatalf("Received != %d, equals %d\n", 4, received)
	}
}

func TestQueueSubsOnReconnect(t *testing.T) {
	ts := startReconnectServer(t)
	defer ts.Shutdown()

	opts := reconnectOpts

	// Allow us to block on reconnect complete.
	reconnectsDone := make(chan bool)
	opts.ReconnectedCB = func(nc *nats.Conn) {
		reconnectsDone <- true
	}

	// Create connection
	nc, err := opts.Connect()
	if err != nil {
		t.Fatalf("Should have connected ok: %v\n", err)
	}
	defer nc.Close()

	ec, err := nats.NewEncodedConn(nc, nats.JSON_ENCODER)
	if err != nil {
		t.Fatalf("Failed to create an encoded connection: %v\n", err)
	}

	// To hold results.
	results := make(map[int]int)
	var mu sync.Mutex

	// Make sure we got what we needed, 1 msg only and all seqnos accounted for..
	checkResults := func(numSent int) {
		mu.Lock()
		defer mu.Unlock()

		for i := 0; i < numSent; i++ {
			if results[i] != 1 {
				t.Fatalf("Received incorrect number of messages, [%d] for seq: %d\n", results[i], i)
			}
		}

		// Auto reset results map
		results = make(map[int]int)
	}

	subj := "foo.bar"
	qgroup := "workers"

	cb := func(seqno int) {
		mu.Lock()
		defer mu.Unlock()
		results[seqno] = results[seqno] + 1
	}

	// Create Queue Subscribers
	ec.QueueSubscribe(subj, qgroup, cb)
	ec.QueueSubscribe(subj, qgroup, cb)

	ec.Flush()

	// Helper function to send messages and check results.
	sendAndCheckMsgs := func(numToSend int) {
		for i := 0; i < numToSend; i++ {
			ec.Publish(subj, i)
		}
		// Wait for processing.
		ec.Flush()
		time.Sleep(50 * time.Millisecond)

		// Check Results
		checkResults(numToSend)
	}

	// Base Test
	sendAndCheckMsgs(10)

	// Stop and restart server
	ts.Shutdown()
	ts = startReconnectServer(t)
	defer ts.Shutdown()

	if err := Wait(reconnectsDone); err != nil {
		t.Fatal("Did not get the ReconnectedCB!")
	}

	// Reconnect Base Test
	sendAndCheckMsgs(10)
}

func TestIsClosed(t *testing.T) {
	ts := startReconnectServer(t)
	defer ts.Shutdown()

	nc := NewConnection(t, TEST_PORT)
	defer nc.Close()

	if nc.IsClosed() {
		t.Fatalf("IsClosed returned true when the connection is still open.")
	}
	ts.Shutdown()
	if nc.IsClosed() {
		t.Fatalf("IsClosed returned true when the connection is still open.")
	}
	ts = startReconnectServer(t)
	defer ts.Shutdown()
	if nc.IsClosed() {
		t.Fatalf("IsClosed returned true when the connection is still open.")
	}
	nc.Close()
	if !nc.IsClosed() {
		t.Fatalf("IsClosed returned false after Close() was called.")
	}
}

func TestIsReconnectingAndStatus(t *testing.T) {
	ts := startReconnectServer(t)
	defer ts.Shutdown()

	disconnectedch := make(chan bool, 3)
	reconnectch := make(chan bool, 2)
	opts := nats.GetDefaultOptions()
	opts.Url = fmt.Sprintf("nats://127.0.0.1:%d", TEST_PORT)
	opts.AllowReconnect = true
	opts.MaxReconnect = 10000
	opts.ReconnectWait = 100 * time.Millisecond
	nats.ReconnectJitter(0, 0)(&opts)

	opts.DisconnectedErrCB = func(_ *nats.Conn, _ error) {
		disconnectedch <- true
	}
	opts.ReconnectedCB = func(_ *nats.Conn) {
		reconnectch <- true
	}

	// Connect, verify initial reconnecting state check, then stop the server
	nc, err := opts.Connect()
	if err != nil {
		t.Fatalf("Should have connected ok: %v", err)
	}
	defer nc.Close()

	if nc.IsReconnecting() {
		t.Fatalf("IsReconnecting returned true when the connection is still open.")
	}
	if status := nc.Status(); status != nats.CONNECTED {
		t.Fatalf("Status returned %d when connected instead of CONNECTED", status)
	}
	ts.Shutdown()

	// Wait until we get the disconnected callback
	if e := Wait(disconnectedch); e != nil {
		t.Fatalf("Disconnect callback wasn't triggered: %v", e)
	}
	if !nc.IsReconnecting() {
		t.Fatalf("IsReconnecting returned false when the client is reconnecting.")
	}
	if status := nc.Status(); status != nats.RECONNECTING {
		t.Fatalf("Status returned %d when reconnecting instead of CONNECTED", status)
	}

	ts = startReconnectServer(t)
	defer ts.Shutdown()

	// Wait until we get the reconnect callback
	if e := Wait(reconnectch); e != nil {
		t.Fatalf("Reconnect callback wasn't triggered: %v", e)
	}
	if nc.IsReconnecting() {
		t.Fatalf("IsReconnecting returned true after the connection was reconnected.")
	}
	if status := nc.Status(); status != nats.CONNECTED {
		t.Fatalf("Status returned %d when reconnected instead of CONNECTED", status)
	}

	// Close the connection, reconnecting should still be false
	nc.Close()
	if nc.IsReconnecting() {
		t.Fatalf("IsReconnecting returned true after Close() was called.")
	}
	if status := nc.Status(); status != nats.CLOSED {
		t.Fatalf("Status returned %d after Close() was called instead of CLOSED", status)
	}
}

func TestFullFlushChanDuringReconnect(t *testing.T) {
	ts := startReconnectServer(t)
	defer ts.Shutdown()

	reconnectch := make(chan bool, 2)

	opts := nats.GetDefaultOptions()
	opts.Url = fmt.Sprintf("nats://127.0.0.1:%d", TEST_PORT)
	opts.AllowReconnect = true
	opts.MaxReconnect = 10000
	opts.ReconnectWait = 100 * time.Millisecond
	nats.ReconnectJitter(0, 0)(&opts)

	opts.ReconnectedCB = func(_ *nats.Conn) {
		reconnectch <- true
	}

	// Connect
	nc, err := opts.Connect()
	if err != nil {
		t.Fatalf("Should have connected ok: %v", err)
	}
	defer nc.Close()

	// Channel used to make the go routine sending messages to stop.
	stop := make(chan bool)

	// While connected, publish as fast as we can
	go func() {
		for i := 0; ; i++ {
			_ = nc.Publish("foo", []byte("hello"))

			// Make sure we are sending at least flushChanSize (1024) messages
			// before potentially pausing.
			if i%2000 == 0 {
				select {
				case <-stop:
					return
				default:
					time.Sleep(100 * time.Millisecond)
				}
			}
		}
	}()

	// Send a bit...
	time.Sleep(500 * time.Millisecond)

	// Shut down the server
	ts.Shutdown()

	// Continue sending while we are disconnected
	time.Sleep(time.Second)

	// Restart the server
	ts = startReconnectServer(t)
	defer ts.Shutdown()

	// Wait for the reconnect CB to be invoked (but not for too long)
	if e := WaitTime(reconnectch, 5*time.Second); e != nil {
		t.Fatalf("Reconnect callback wasn't triggered: %v", e)
	}
	close(stop)
}

func TestReconnectVerbose(t *testing.T) {
	s := RunDefaultServer()
	defer s.Shutdown()

	o := nats.GetDefaultOptions()
	o.ReconnectWait = 50 * time.Millisecond
	o.Verbose = true
	rch := make(chan bool)
	o.ReconnectedCB = func(_ *nats.Conn) {
		rch <- true
	}

	nc, err := o.Connect()
	if err != nil {
		t.Fatalf("Should have connected ok: %v", err)
	}
	defer nc.Close()

	err = nc.Flush()
	if err != nil {
		t.Fatalf("Error during flush: %v", err)
	}

	s.Shutdown()
	s = RunDefaultServer()
	defer s.Shutdown()

	if e := Wait(rch); e != nil {
		t.Fatal("Should have reconnected ok")
	}

	err = nc.Flush()
	if err != nil {
		t.Fatalf("Error during flush: %v", err)
	}
}

func TestReconnectBufSizeOption(t *testing.T) {
	s := RunDefaultServer()
	defer s.Shutdown()

	nc, err := nats.Connect("nats://127.0.0.1:4222", nats.ReconnectBufSize(32))
	if err != nil {
		t.Fatalf("Should have connected ok: %v", err)
	}
	defer nc.Close()

	if nc.Opts.ReconnectBufSize != 32 {
		t.Fatalf("ReconnectBufSize should be 32 but it is %d", nc.Opts.ReconnectBufSize)
	}
}

func TestReconnectBufSize(t *testing.T) {
	s := RunDefaultServer()
	defer s.Shutdown()

	o := nats.GetDefaultOptions()
	o.ReconnectBufSize = 32 // 32 bytes

	dch := make(chan bool)
	o.DisconnectedErrCB = func(_ *nats.Conn, _ error) {
		dch <- true
	}

	nc, err := o.Connect()
	if err != nil {
		t.Fatalf("Should have connected ok: %v", err)
	}
	defer nc.Close()

	err = nc.Flush()
	if err != nil {
		t.Fatalf("Error during flush: %v", err)
	}

	// Force disconnected state.
	s.Shutdown()

	if e := Wait(dch); e != nil {
		t.Fatal("DisconnectedErrCB should have been triggered")
	}

	msg := []byte("food") // 4 bytes paylaod, total proto is 16 bytes
	// These should work, 2X16 = 32
	if err := nc.Publish("foo", msg); err != nil {
		t.Fatalf("Failed to publish message: %v\n", err)
	}
	if err := nc.Publish("foo", msg); err != nil {
		t.Fatalf("Failed to publish message: %v\n", err)
	}

	// This should fail since we have exhausted the backing buffer.
	if err := nc.Publish("foo", msg); err == nil {
		t.Fatalf("Expected to fail to publish message: got no error\n")
	}
	nc.Buffered()
}

// When a cluster is fronted by a single DNS name (desired) but communicates IPs to clients (also desired),
// and we use TLS, we want to make sure we do the right thing connecting to an IP directly for TLS to work.
// The reason this may happen is that the cluster has a single DNS name and a single certificate, but the cluster
// wants to vend out IPs and not wait on DNS for topology changes and failover.
func TestReconnectTLSHostNoIP(t *testing.T) {
	sa, optsA := RunServerWithConfig("./configs/tls_noip_a.conf")
	defer sa.Shutdown()
	sb, optsB := RunServerWithConfig("./configs/tls_noip_b.conf")
	defer sb.Shutdown()

	// Wait for cluster to form.
	wait := time.Now().Add(2 * time.Second)
	for time.Now().Before(wait) {
		sanr := sa.NumRoutes()
		sbnr := sb.NumRoutes()
		if sanr == 1 && sbnr == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	endpoint := fmt.Sprintf("%s:%d", optsA.Host, optsA.Port)
	secureURL := fmt.Sprintf("tls://%s:%s@%s/", optsA.Username, optsA.Password, endpoint)

	dch := make(chan bool, 2)
	dcb := func(_ *nats.Conn, _ error) { dch <- true }
	rch := make(chan bool)
	rcb := func(_ *nats.Conn) { rch <- true }

	nc, err := nats.Connect(secureURL,
		nats.RootCAs("./configs/certs/ca.pem"),
		nats.DisconnectErrHandler(dcb),
		nats.ReconnectHandler(rcb))
	if err != nil {
		t.Fatalf("Failed to create secure (TLS) connection: %v", err)
	}
	defer nc.Close()

	// Wait for DiscoveredServers() to be 1.
	wait = time.Now().Add(2 * time.Second)
	for time.Now().Before(wait) {
		if len(nc.DiscoveredServers()) == 1 {
			break
		}
	}
	// Make sure this is the server B info, and that it is an IP.
	expectedDiscoverURL := fmt.Sprintf("tls://%s:%d", optsB.Host, optsB.Port)
	eurl, err := url.Parse(expectedDiscoverURL)
	if err != nil {
		t.Fatalf("Expected to parse discovered server URL: %v", err)
	}
	if addr := net.ParseIP(eurl.Hostname()); addr == nil {
		t.Fatalf("Expected the discovered server to be an IP, got %v", eurl.Hostname())
	}
	ds := nc.DiscoveredServers()
	if ds[0] != expectedDiscoverURL {
		t.Fatalf("Expected %q, got %q", expectedDiscoverURL, ds[0])
	}

	// Force us to switch servers.
	sa.Shutdown()

	if e := Wait(dch); e != nil {
		t.Fatal("DisconnectedErrCB should have been triggered")
	}
	if e := WaitTime(rch, time.Second); e != nil {
		t.Fatalf("ReconnectedCB should have been triggered: %v", nc.LastError())
	}
}

var reconnectOpts = nats.Options{
	Url:            fmt.Sprintf("nats://127.0.0.1:%d", TEST_PORT),
	AllowReconnect: true,
	MaxReconnect:   10,
	ReconnectWait:  100 * time.Millisecond,
	Timeout:        nats.DefaultTimeout,
}

func TestConnCloseNoCallback(t *testing.T) {
	ts := startReconnectServer(t)
	defer ts.Shutdown()

	// create a connection that manually sets the options
	var conns []*nats.Conn
	cch := make(chan string, 2)
	opts := reconnectOpts
	opts.ClosedCB = func(_ *nats.Conn) {
		cch <- "manual"
	}
	opts.NoCallbacksAfterClientClose = true
	nc, err := opts.Connect()
	if err != nil {
		t.Fatalf("Should have connected ok: %v", err)
	}
	conns = append(conns, nc)

	// and another connection that uses the option
	nc2, err := nats.Connect(reconnectOpts.Url, nats.NoCallbacksAfterClientClose(),
		nats.ClosedHandler(func(_ *nats.Conn) {
			cch <- "opts"
		}))
	if err != nil {
		t.Fatalf("Should have connected ok: %v", err)
	}
	conns = append(conns, nc2)

	// defer close() for safety, flush() and close()
	for _, c := range conns {
		defer c.Close()
		c.Flush()

		// Close the connection, we don't expect to get a notification
		c.Close()
	}

	// if the timeout happens we didn't get data from the channel
	// if we get a value from the channel that connection type failed.
	select {
	case <-time.After(500 * time.Millisecond):
		// test passed - we timed so no callback was called
	case what := <-cch:
		t.Fatalf("%s issued a callback and it shouldn't have", what)
	}
}

func TestReconnectBufSizeDisable(t *testing.T) {
	s := RunDefaultServer()
	defer s.Shutdown()

	o := nats.GetDefaultOptions()

	// Disable buffering to always get a synchronous error when publish fails.
	o.ReconnectBufSize = -1

	dch := make(chan bool)
	o.DisconnectedErrCB = func(_ *nats.Conn, _ error) {
		dch <- true
	}

	nc, err := o.Connect()
	if err != nil {
		t.Fatalf("Should have connected ok: %v", err)
	}
	defer nc.Close()

	err = nc.Flush()
	if err != nil {
		t.Fatalf("Error during flush: %v", err)
	}

	// Force disconnected state.
	s.Shutdown()

	if e := Wait(dch); e != nil {
		t.Fatal("DisconnectedErrCB should have been triggered")
	}

	msg := []byte("food")
	if err := nc.Publish("foo", msg); err != nats.ErrReconnectBufExceeded {
		t.Fatalf("Unexpected error: %v\n", err)
	}
	got, _ := nc.Buffered()
	if got != 0 {
		t.Errorf("Unexpected buffered bytes: %v", got)
	}
}
