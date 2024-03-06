package outbound

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"github.com/metacubex/mihomo/log"
	"math/rand"
	"net"
	"runtime"
	"strconv"
	"time"

	CN "github.com/metacubex/mihomo/common/net"
	"github.com/metacubex/mihomo/component/ca"
	"github.com/metacubex/mihomo/component/dialer"
	"github.com/metacubex/mihomo/component/proxydialer"
	C "github.com/metacubex/mihomo/constant"
	tuicCommon "github.com/metacubex/mihomo/transport/tuic/common"

	"github.com/metacubex/sing-quic/hysteria2"

	M "github.com/sagernet/sing/common/metadata"
)

func init() {
	hysteria2.SetCongestionController = tuicCommon.SetCongestionController
}

type Hysteria2 struct {
	*Base

	option         *Hysteria2Option
	client         *hysteria2.Client
	dialer         proxydialer.SingDialer
	needUpdateDial bool
	DialUpdateAt   int64
	ListenUpdateAt int64
	cclient        *hysteria2.Client
	ticker         *time.Ticker
	tickerDone     chan struct{}
}

type Hysteria2Option struct {
	BasicOption
	Name           string   `proxy:"name"`
	Server         string   `proxy:"server"`
	Port           int      `proxy:"port"`
	Up             string   `proxy:"up,omitempty"`
	Down           string   `proxy:"down,omitempty"`
	Password       string   `proxy:"password,omitempty"`
	Obfs           string   `proxy:"obfs,omitempty"`
	ObfsPassword   string   `proxy:"obfs-password,omitempty"`
	SNI            string   `proxy:"sni,omitempty"`
	SkipCertVerify bool     `proxy:"skip-cert-verify,omitempty"`
	Fingerprint    string   `proxy:"fingerprint,omitempty"`
	ALPN           []string `proxy:"alpn,omitempty"`
	CustomCA       string   `proxy:"ca,omitempty"`
	CustomCAString string   `proxy:"ca-str,omitempty"`
	CWND           int      `proxy:"cwnd,omitempty"`
	UdpMTU         int      `proxy:"udp-mtu,omitempty"`
}

func (h *Hysteria2) DialContext(ctx context.Context, metadata *C.Metadata, opts ...dialer.Option) (_ C.Conn, err error) {

	if h.needUpdateDial {
		h.client = h.cclient
		h.needUpdateDial = false
		h.DialUpdateAt = time.Now().UnixMilli()
		h.ListenUpdateAt = h.DialUpdateAt
	}

	options := h.Base.DialOptions(opts...)
	h.dialer.SetDialer(dialer.NewDialer(options...))
	c, err := h.client.DialConn(ctx, M.ParseSocksaddrHostPort(metadata.String(), metadata.DstPort))
	if err != nil {
		return nil, err
	}
	return NewConn(CN.NewRefConn(c, h), h), nil
}

func (h *Hysteria2) ListenPacketContext(ctx context.Context, metadata *C.Metadata, opts ...dialer.Option) (_ C.PacketConn, err error) {

	switch {
	case h.ListenUpdateAt == h.DialUpdateAt:
		h.ListenUpdateAt++
	case h.ListenUpdateAt == h.DialUpdateAt+1:
		h.client = h.cclient
		h.ListenUpdateAt++
	default:

	}

	options := h.Base.DialOptions(opts...)
	h.dialer.SetDialer(dialer.NewDialer(options...))
	pc, err := h.client.ListenPacket(ctx)
	if err != nil {
		return nil, err
	}
	if pc == nil {
		return nil, errors.New("packetConn is nil")
	}
	return newPacketConn(CN.NewRefPacketConn(CN.NewThreadSafePacketConn(pc), h), h), nil
}

func closeHysteria2(h *Hysteria2) {

	h.ticker.Stop()
	h.tickerDone <- struct{}{}
	if h.client != nil {
		_ = h.client.CloseWithError(errors.New("proxy removed"))
	}
}

func NewHysteria2(option Hysteria2Option) (*Hysteria2, error) {
	addr := net.JoinHostPort(option.Server, strconv.Itoa(option.Port))
	//var salamanderPassword string
	if len(option.Obfs) > 0 {
		if option.ObfsPassword == "" {
			return nil, errors.New("missing obfs password")
		}
		switch option.Obfs {
		case hysteria2.ObfsTypeSalamander:
			//salamanderPassword = option.ObfsPassword
		default:
			return nil, fmt.Errorf("unknown obfs type: %s", option.Obfs)
		}
	}

	serverName := option.Server
	if option.SNI != "" {
		serverName = option.SNI
	}

	tlsConfig := &tls.Config{
		ServerName:         serverName,
		InsecureSkipVerify: option.SkipCertVerify,
		MinVersion:         tls.VersionTLS13,
	}

	var err error
	tlsConfig, err = ca.GetTLSConfig(tlsConfig, option.Fingerprint, option.CustomCA, option.CustomCAString)
	if err != nil {
		return nil, err
	}

	if len(option.ALPN) > 0 {
		tlsConfig.NextProtos = option.ALPN
	}

	if option.UdpMTU == 0 {
		// "1200" from quic-go's MaxDatagramSize
		// "-3" from quic-go's DatagramFrame.MaxDataLen
		option.UdpMTU = 1200 - 3
	}

	singDialer := proxydialer.NewByNameSingDialer(option.DialerProxy, dialer.NewDialer())

	clientOptions := hysteria2.ClientOptions{
		Context:            context.TODO(),
		Dialer:             singDialer,
		ServerAddress:      M.ParseSocksaddrHostPort(option.Server, uint16(50005+rand.Int63n(int64(4000)))),
		SendBPS:            StringToBps(option.Up),
		ReceiveBPS:         StringToBps(option.Down),
		SalamanderPassword: "",
		Password:           option.Password,
		TLSConfig:          tlsConfig,
		UDPDisabled:        false,
		CWND:               option.CWND,
	}

	client, err := hysteria2.NewClient(clientOptions)
	if err != nil {
		return nil, err
	}

	outbound := &Hysteria2{
		Base: &Base{
			name:   option.Name,
			addr:   addr,
			tp:     C.Hysteria2,
			udp:    true,
			iface:  option.Interface,
			rmark:  option.RoutingMark,
			prefer: C.NewDNSPrefer(option.IPVersion),
		},
		option: &option,
		client: client,
		dialer: singDialer,
		ticker: time.NewTicker(30 * time.Second),
	}

	_ = outbound.createNewClient(tlsConfig)
	go func(h *Hysteria2, tlsConfig *tls.Config) {
		for {
			select {
			case <-h.ticker.C:
				_ = h.createNewClient(tlsConfig)
			case <-h.tickerDone:
				log.Errorln("done the ticker")
				return
			}
		}

	}(outbound, tlsConfig)

	runtime.SetFinalizer(outbound, closeHysteria2)

	return outbound, nil
}
func (h *Hysteria2) createNewClient(tlsConfig *tls.Config) (err error) {
	port := 50005 + rand.Int63n(int64(4000))
	//singDialer := proxydialer.NewByNameSingDialer(h.option.DialerProxy, dialer.NewDialer())
	clientOptions := hysteria2.ClientOptions{
		Context:            context.TODO(),
		Dialer:             h.dialer,
		ServerAddress:      M.ParseSocksaddrHostPort(h.option.Server, uint16(port)),
		SendBPS:            StringToBps(h.option.Up),
		ReceiveBPS:         StringToBps(h.option.Down),
		SalamanderPassword: "",
		Password:           h.option.Password,
		TLSConfig:          tlsConfig,
		UDPDisabled:        false,
		CWND:               h.option.CWND,
	}
	h.cclient, err = hysteria2.NewClient(clientOptions)
	if err != nil {
		return err
	}
	h.needUpdateDial = true
	log.Errorln("new port%+v\n", port)
	return err
}
