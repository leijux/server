package clients

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/xid"

	"github.com/mochi-co/mqtt/internal/auth"
	"github.com/mochi-co/mqtt/internal/circ"
	"github.com/mochi-co/mqtt/internal/packets"
	"github.com/mochi-co/mqtt/internal/topics"
)

var (
	defaultKeepalive uint16 = 10                                              // in seconds.
	resendBackoff           = []int64{0, 1, 2, 10, 60, 120, 600, 3600, 21600} // <1 second to 6 hours
	maxResends              = 6                                               // maximum number of times to retry sending QoS packets.

	ErrConnectionClosed = errors.New("Connection not open")
)

// Clients contains a map of the clients known by the broker.
type Clients struct {
	sync.RWMutex
	internal map[string]*Client // clients known by the broker, keyed on client id.
}

// New returns an instance of Clients.
func New() Clients {
	return Clients{
		internal: make(map[string]*Client),
	}
}

// Add adds a new client to the clients map, keyed on client id.
func (cl *Clients) Add(val *Client) {
	cl.Lock()
	cl.internal[val.ID] = val
	cl.Unlock()
}

// Get returns the value of a client if it exists.
func (cl *Clients) Get(id string) (*Client, bool) {
	cl.RLock()
	val, ok := cl.internal[id]
	cl.RUnlock()
	return val, ok
}

// Len returns the length of the clients map.
func (cl *Clients) Len() int {
	cl.RLock()
	val := len(cl.internal)
	cl.RUnlock()
	return val
}

// Delete removes a client from the internal map.
func (cl *Clients) Delete(id string) {
	cl.Lock()
	delete(cl.internal, id)
	cl.Unlock()
}

// GetByListener returns clients matching a listener id.
func (cl *Clients) GetByListener(id string) []*Client {
	clients := make([]*Client, 0, cl.Len())
	cl.RLock()
	for _, v := range cl.internal {
		if v.Listener == id && atomic.LoadInt64(&v.state.done) == 0 {
			clients = append(clients, v)
		}
	}
	cl.RUnlock()
	return clients
}

// Client contains information about a client known by the broker.
type Client struct {
	sync.RWMutex
	conn          net.Conn             // the net.Conn used to establish the connection.
	r             *circ.Reader         // a reader for reading incoming bytes.
	w             *circ.Writer         // a writer for writing outgoing bytes.
	ID            string               // the client id.
	AC            auth.Controller      // an auth controller inherited from the listener.
	Subscriptions topics.Subscriptions // a map of the subscription filters a client maintains.
	Listener      string               // the id of the listener the client is connected to.
	InFlight      InFlight             // a map of in-flight qos messages.
	Username      []byte               // the username the client authenticated with.
	keepalive     uint16               // the number of seconds the connection can wait.
	cleanSession  bool                 // indicates if the client expects a clean-session.
	packetID      uint32               // the current highest packetID.
	LWT           LWT                  // the last will and testament for the client.
	state         clientState          // the operational state of the client.
}

// clientState tracks the state of the client.
type clientState struct {
	started *sync.WaitGroup // tracks the goroutines which have been started.
	endedW  *sync.WaitGroup // tracks when the writer has ended.
	endedR  *sync.WaitGroup // tracks when the reader has ended.
	done    int64           // atomic counter which indicates that the client has closed.
	endOnce sync.Once       //
}

// NewClient returns a new instance of Client.
func NewClient(c net.Conn, r *circ.Reader, w *circ.Writer) *Client {
	cl := &Client{
		conn: c,
		r:    r,
		w:    w,

		keepalive: defaultKeepalive,
		InFlight: InFlight{
			internal: make(map[uint16]InFlightMessage),
		},
		Subscriptions: make(map[string]byte),

		state: clientState{
			started: new(sync.WaitGroup),
			endedW:  new(sync.WaitGroup),
			endedR:  new(sync.WaitGroup),
		},
	}

	cl.refreshDeadline(cl.keepalive)

	return cl
}

// Identify sets the identification values of a client instance.
func (cl *Client) Identify(lid string, pk packets.Packet, ac auth.Controller) {
	cl.Listener = lid
	cl.AC = ac

	cl.ID = pk.ClientIdentifier
	if cl.ID == "" {
		cl.ID = xid.New().String()
	}

	cl.r.ID = cl.ID + " READER"
	cl.w.ID = cl.ID + " WRITER"

	cl.Username = pk.Username
	cl.cleanSession = pk.CleanSession
	cl.keepalive = pk.Keepalive

	if pk.WillFlag {
		cl.LWT = LWT{
			Topic:   pk.WillTopic,
			Message: pk.WillMessage,
			Qos:     pk.WillQos,
			Retain:  pk.WillRetain,
		}
	}

	cl.refreshDeadline(cl.keepalive)
}

// refreshDeadline refreshes the read/write deadline for the net.Conn connection.
func (cl *Client) refreshDeadline(keepalive uint16) {
	if cl.conn != nil {
		var expiry time.Time // Nil time can be used to disable deadline if keepalive = 0
		if keepalive > 0 {
			expiry = time.Now().Add(time.Duration(keepalive+(keepalive/2)) * time.Second)
		}
		cl.conn.SetDeadline(expiry)
	}
}

// NextPacketID returns the next packet id for a client, looping back to 0
// if the maximum ID has been reached.
func (cl *Client) NextPacketID() uint32 {
	i := atomic.LoadUint32(&cl.packetID)
	if i == uint32(65535) || i == uint32(0) {
		atomic.StoreUint32(&cl.packetID, 1)
		return 1
	}

	return atomic.AddUint32(&cl.packetID, 1)
}

// NoteSubscription makes a note of a subscription for the client.
func (cl *Client) NoteSubscription(filter string, qos byte) {
	cl.Lock()
	cl.Subscriptions[filter] = qos
	cl.Unlock()
}

// ForgetSubscription forgests a subscription note for the client.
func (cl *Client) ForgetSubscription(filter string) {
	cl.Lock()
	delete(cl.Subscriptions, filter)
	cl.Unlock()
}

// Start begins the client goroutines reading and writing packets.
func (cl *Client) Start() {
	cl.state.started.Add(2)

	go func() {
		cl.state.started.Done()
		cl.w.WriteTo(cl.conn)
		cl.state.endedW.Done()
		cl.Stop()
	}()
	cl.state.endedW.Add(1)

	go func() {
		cl.state.started.Done()
		cl.r.ReadFrom(cl.conn)
		cl.state.endedR.Done()
		cl.Stop()
	}()
	cl.state.endedR.Add(1)

	cl.state.started.Wait()
}

// Stop instructs the client to shut down all processing goroutines and disconnect.
func (cl *Client) Stop() {
	cl.state.endOnce.Do(func() {
		cl.r.Stop()
		cl.w.Stop()
		cl.state.endedW.Wait()

		//fmt.Println("closing conn")
		//cl.conn.Close()

		cl.state.endedR.Wait()
		atomic.StoreInt64(&cl.state.done, 1)
	})
}

// readFixedHeader reads in the values of the next packet's fixed header.
func (cl *Client) ReadFixedHeader(fh *packets.FixedHeader) error {
	p, err := cl.r.Read(1)
	if err != nil {
		return err
	}

	err = fh.Decode(p[0])
	if err != nil {
		return err
	}

	// The remaining length value can be up to 5 bytes. Read through each byte
	// looking for continue values, and if found increase the read. Otherwise
	// decode the bytes that were legit.
	buf := make([]byte, 0, 6)
	i := 1
	n := 2
	for ; n < 6; n++ {
		p, err = cl.r.Read(n)
		if err != nil {
			return err
		}

		buf = append(buf, p[i])

		// If it's not a continuation flag, end here.
		if p[i] < 128 {
			break
		}

		// If i has reached 4 without a length terminator, return a protocol violation.
		i++
		if i == 4 {
			return packets.ErrOversizedLengthIndicator
		}
	}

	// Calculate and store the remaining length of the packet payload.
	rem, _ := binary.Uvarint(buf)
	fh.Remaining = int(rem)

	// Having successfully read n bytes, commit the tail forward.
	cl.r.CommitTail(n)

	return nil
}

// Read reads new packets from a client connection
func (cl *Client) Read(h func(*Client, packets.Packet) error) error {
	for {
		if atomic.LoadInt64(&cl.state.done) == 1 && cl.r.CapDelta() == 0 {
			return nil
		}

		cl.refreshDeadline(cl.keepalive)
		fh := new(packets.FixedHeader)
		err := cl.ReadFixedHeader(fh)
		if err != nil {
			return err
		}

		pk, err := cl.ReadPacket(fh)
		if err != nil {
			return err
		}

		err = h(cl, pk) // Process inbound packet.
		if err != nil {
			return err
		}

		// Attempt periodic resend of inflight messages (where applicable).
		//cl.ResendInflight(false)
	}
}

// ResendInflight will attempt resend send any in-flight messages stored for a client.
func (cl *Client) ResendInflight(force bool) error {
	if cl.InFlight.Len() == 0 {
		return nil
	}

	fmt.Println("\n", cl.ID, "## Resending Inflight")
	nt := time.Now().Unix()
	for _, tk := range cl.InFlight.GetAll() {

		if tk.resends >= maxResends { // After a reasonable time, drop inflight packets.
			cl.InFlight.Delete(tk.Packet.PacketID)
			continue
		}

		// Only continue if the resend backoff time has passed and there's a backoff time.
		if !force && (nt-tk.Sent < resendBackoff[tk.resends] || len(resendBackoff) < tk.resends) {
			continue
		}

		if tk.Packet.FixedHeader.Type == packets.Publish {
			tk.Packet.FixedHeader.Dup = true
		}

		tk.resends++
		tk.Sent = nt
		cl.InFlight.Set(tk.Packet.PacketID, tk)

		fmt.Println(cl.ID, ">", tk.Packet.FixedHeader.Type, tk.Packet.FixedHeader.Qos, tk.Packet.PacketID)
		_, err := cl.WritePacket(tk.Packet)
		//err := h(cl, tk.Packet)
		if err != nil {
			return err
		}
	}

	return nil
}

// ReadPacket reads the remaining buffer into an MQTT packet.
func (cl *Client) ReadPacket(fh *packets.FixedHeader) (pk packets.Packet, err error) {
	pk.FixedHeader = *fh
	if pk.FixedHeader.Remaining == 0 {
		return
	}

	p, err := cl.r.Read(pk.FixedHeader.Remaining)
	if err != nil {
		return pk, err
	}

	// Decode the remaining packet values using a fresh copy of the bytes,
	// otherwise the next packet will change the data of this one.
	px := append([]byte{}, p[:]...)

	switch pk.FixedHeader.Type {
	case packets.Connect:
		err = pk.ConnectDecode(px)
	case packets.Connack:
		err = pk.ConnackDecode(px)
	case packets.Publish:
		err = pk.PublishDecode(px)
	case packets.Puback:
		err = pk.PubackDecode(px)
	case packets.Pubrec:
		err = pk.PubrecDecode(px)
	case packets.Pubrel:
		err = pk.PubrelDecode(px)
	case packets.Pubcomp:
		err = pk.PubcompDecode(px)
	case packets.Subscribe:
		err = pk.SubscribeDecode(px)
	case packets.Suback:
		err = pk.SubackDecode(px)
	case packets.Unsubscribe:
		err = pk.UnsubscribeDecode(px)
	case packets.Unsuback:
		err = pk.UnsubackDecode(px)
	case packets.Pingreq:
	case packets.Pingresp:
	case packets.Disconnect:
	default:
		err = fmt.Errorf("No valid packet available; %v", pk.FixedHeader.Type)
	}

	cl.r.CommitTail(pk.FixedHeader.Remaining)

	return
}

// WritePacket encodes and writes a packet to the client.
func (cl *Client) WritePacket(pk packets.Packet) (n int, err error) {
	if atomic.LoadInt64(&cl.state.done) == 1 {
		return 0, ErrConnectionClosed
	}

	cl.w.Mu.Lock()
	defer cl.w.Mu.Unlock()

	buf := new(bytes.Buffer)
	switch pk.FixedHeader.Type {
	case packets.Connect:
		err = pk.ConnectEncode(buf)
	case packets.Connack:
		err = pk.ConnackEncode(buf)
	case packets.Publish:
		err = pk.PublishEncode(buf)
	case packets.Puback:
		err = pk.PubackEncode(buf)
	case packets.Pubrec:
		err = pk.PubrecEncode(buf)
	case packets.Pubrel:
		err = pk.PubrelEncode(buf)
	case packets.Pubcomp:
		err = pk.PubcompEncode(buf)
	case packets.Subscribe:
		err = pk.SubscribeEncode(buf)
	case packets.Suback:
		err = pk.SubackEncode(buf)
	case packets.Unsubscribe:
		err = pk.UnsubscribeEncode(buf)
	case packets.Unsuback:
		err = pk.UnsubackEncode(buf)
	case packets.Pingreq:
		err = pk.PingreqEncode(buf)
	case packets.Pingresp:
		err = pk.PingrespEncode(buf)
	case packets.Disconnect:
		err = pk.DisconnectEncode(buf)
	default:
		err = fmt.Errorf("No valid packet available; %v", pk.FixedHeader.Type)
	}
	if err != nil {
		return
	}

	n, err = cl.w.Write(buf.Bytes())
	if err != nil {
		return
	}

	cl.refreshDeadline(cl.keepalive)

	return
}

// LWT contains the last will and testament details for a client connection.
type LWT struct {
	Topic   string // the topic the will message shall be sent to.
	Message []byte // the message that shall be sent when the client disconnects.
	Qos     byte   // the quality of service desired.
	Retain  bool   // indicates whether the will message should be retained
}

// InFlightMessage contains data about a packet which is currently in-flight.
type InFlightMessage struct {
	Packet  packets.Packet // the packet currently in-flight.
	Sent    int64          // the last time the message was sent (for retries) in unixtime.
	resends int            // the number of times the message was attempted to be sent.
}

// InFlight is a map of InFlightMessage keyed on packet id.
type InFlight struct {
	sync.RWMutex
	internal map[uint16]InFlightMessage // internal contains the inflight messages.
}

// Set stores the packet of an in-flight message, keyed on message id.
func (i *InFlight) Set(key uint16, in InFlightMessage) {
	i.Lock()
	i.internal[key] = in
	i.Unlock()
}

// Get returns the value of an in-flight message if it exists.
func (i *InFlight) Get(key uint16) (InFlightMessage, bool) {
	i.RLock()
	val, ok := i.internal[key]
	i.RUnlock()
	return val, ok
}

// Len returns the size of the inflight messages map.
func (i *InFlight) Len() int {
	i.RLock()
	v := len(i.internal)
	i.RUnlock()
	return v
}

// GetAll returns all the inflight messages.
func (i *InFlight) GetAll() map[uint16]InFlightMessage {
	i.RLock()
	defer i.RUnlock()
	return i.internal
}

// Delete removes an in-flight message from the map.
func (i *InFlight) Delete(key uint16) {
	i.Lock()
	delete(i.internal, key)
	i.Unlock()
}
