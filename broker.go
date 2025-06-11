package main

import (
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"rttys/client"
	"rttys/config"
	"rttys/utils"

	"github.com/gorilla/websocket"
	jsoniter "github.com/json-iterator/go"
	"github.com/rs/zerolog/log"
)

type session struct {
	dev       client.Client
	user      client.Client
	confirmed uint32
}

type broker struct {
	cfg         *config.Config
	devices     sync.Map
	loginAck    chan *loginAckMsg
	logout      chan string
	register    chan client.Client
	unregister  chan client.Client
	sessions    sync.Map
	termMessage chan *termMessage
	fileMessage chan *fileMessage
	userMessage chan *usrMessage
	heartbeat   chan string
	cmdResp     chan []byte
	cmdReq      chan *commandReq
	httpResp    chan *httpResp
	httpReq     chan *httpReq
	fileProxy   sync.Map
	devCertPool *x509.CertPool
}

func newBroker(cfg *config.Config) *broker {
	return &broker{
		cfg:         cfg,
		loginAck:    make(chan *loginAckMsg, 1000),
		logout:      make(chan string, 1000),
		register:    make(chan client.Client, 1000),
		unregister:  make(chan client.Client, 1000),
		termMessage: make(chan *termMessage, 1000),
		fileMessage: make(chan *fileMessage, 1000),
		userMessage: make(chan *usrMessage, 1000),
		heartbeat:   make(chan string, 1000),
		cmdResp:     make(chan []byte, 1000),
		cmdReq:      make(chan *commandReq, 1000),
		httpResp:    make(chan *httpResp, 1000),
		httpReq:     make(chan *httpReq, 1000),
	}
}

func devAuth(cfg *config.Config, dev *device) bool {
	if cfg.Token != "" && dev.token != cfg.Token {
		return false
	}

	if cfg.DevHookUrl == "" {
		return true
	}

	cli := &http.Client{
		Timeout: 3 * time.Second,
	}

	data := fmt.Sprintf(`{"devid":"%s", "token":"%s"}`, dev.id, dev.token)
	resp, err := cli.Post(cfg.DevHookUrl, "application/json", strings.NewReader(data))
	if err != nil {
		log.Error().Msg("call device hook url fail:" + err.Error())
		return false
	}
	defer resp.Body.Close()

	return resp.StatusCode == http.StatusOK
}

func (br *broker) getDevice(devid string) (*device, bool) {
	if dev, ok := br.devices.Load(devid); ok {
		return dev.(*device), true
	}
	return nil, false
}

func (br *broker) getSession(sid string) (*session, bool) {
	if dev, ok := br.sessions.Load(sid); ok {
		return dev.(*session), true
	}
	return nil, false
}

func (br *broker) run() {
	for {
		select {
		case c := <-br.register:
			if c.Closed() {
				break
			}

			devid := c.DeviceID()

			if c.IsDevice() {
				dev := c.(*device)
				err := byte(0)
				msg := "OK"

				if _, ok := br.getDevice(devid); ok {
					log.Error().Msg("Device ID conflicting: " + devid)
					msg = "ID conflicting"
					err = 1
				} else if !devAuth(br.cfg, dev) {
					log.Error().Msgf("%s: auth failed", devid)
					msg = "auth failed"
					err = 1
				} else if dev.proto < rttyProtoRequired {
					if dev.proto < rttyProtoRequired {
						log.Error().Msgf("%s: unsupported protocol version: %d, need %d", dev.id, dev.proto, rttyProtoRequired)
						msg = "unsupported protocol"
						err = 1
					}
				} else {
					dev.registered = true
					br.devices.Store(devid, c)
					log.Info().Msgf("Device '%s' registered, proto %d, heartbeat %v", devid, dev.proto, dev.heartbeat)
				}

				c.WriteMsg(msgTypeRegister, append([]byte{err}, msg...))

				if err > 0 {
					// ensure the last packet was sent
					time.AfterFunc(time.Millisecond*100, func() {
						dev.Close()
					})
				}
			} else {
				if dev, ok := br.getDevice(devid); ok {
					sid := utils.GenUniqueID("sid")

					c.(*user).sid = sid

					s := &session{
						dev:  dev,
						user: c,
					}

					time.AfterFunc(time.Second*3, func() {
						if atomic.LoadUint32(&s.confirmed) == 0 {
							log.Error().Msgf("Session '%s' confirm timeout", sid)
							c.CloseConn()
						}
					})

					br.sessions.Store(sid, s)

					dev.WriteMsg(msgTypeLogin, []byte(sid))
					log.Info().Msg("New session: " + sid)
				} else {
					userLoginAck(loginErrorOffline, c)
					log.Error().Msgf("Not found the device '%s'", devid)
				}
			}

		case c := <-br.unregister:
			devid := c.DeviceID()

			c.Close()

			if c.IsDevice() {
				dev := c.(*device)

				if !dev.registered {
					break
				}

				br.devices.Delete(devid)

				dev.registered = false

				br.sessions.Range(func(key, value any) bool {
					sid := key.(string)
					s := value.(*session)

					if s.dev == c {
						br.sessions.Delete(sid)
						s.user.Close()
						log.Info().Msg("Delete session: " + sid)
					}
					return true
				})

				log.Info().Msgf("Device '%s' unregistered", devid)
			} else {
				sid := c.(*user).sid

				if _, ok := br.getSession(sid); ok {
					br.sessions.Delete(sid)

					if dev, ok := br.getDevice(devid); ok {
						dev.WriteMsg(msgTypeLogout, []byte(sid))
					}

					log.Info().Msg("Delete session: " + sid)
				}
			}

		case msg := <-br.loginAck:
			if s, ok := br.getSession(msg.sid); ok {
				if msg.isBusy {
					userLoginAck(loginErrorBusy, s.user)
					log.Error().Msg("login fail, device busy")
				} else {
					atomic.StoreUint32(&s.confirmed, 1)

					userLoginAck(loginErrorNone, s.user)
				}
			}

		// device active logout
		// typically, executing the exit command at the terminal will case this
		case sid := <-br.logout:
			if s, ok := br.getSession(sid); ok {
				br.sessions.Delete(sid)
				s.user.Close()

				log.Info().Msg("Delete session: " + sid)
			}

		case msg := <-br.termMessage:
			if s, ok := br.getSession(msg.sid); ok {
				s.user.WriteMsg(websocket.BinaryMessage, msg.data)
			}

		case msg := <-br.fileMessage:
			sid := msg.sid
			if s, ok := br.getSession(sid); ok {
				typ := msg.data[0]
				data := msg.data[1:]

				switch typ {
				case msgTypeFileSend:
					pipereader, pipewriter := io.Pipe()
					br.fileProxy.Store(sid, &fileProxy{pipereader, pipewriter})
					s.user.WriteMsg(websocket.TextMessage, fmt.Appendf(nil, `{"type":"sendfile", "name": "%s"}`, string(data)))

				case msgTypeFileRecv:
					s.user.WriteMsg(websocket.TextMessage, []byte(`{"type":"recvfile"}`))

				case msgTypeFileData:
					if fp, ok := br.fileProxy.Load(sid); ok {
						fp := fp.(*fileProxy)
						if len(data) == 0 {
							fp.Close()
						} else {
							fp.Write(s.dev, sid, data)
						}
					}

				case msgTypeFileAck:
					s.user.WriteMsg(websocket.TextMessage, []byte(`{"type":"fileAck"}`))

				case msgTypeFileAbort:
					if fp, ok := br.fileProxy.Load(sid); ok {
						fp := fp.(*fileProxy)
						fp.Close()
					}
				}
			}

		case msg := <-br.userMessage:
			if s, ok := br.getSession(msg.sid); ok {
				if dev, ok := br.getDevice(s.dev.DeviceID()); ok {
					data := msg.data

					if msg.typ == websocket.BinaryMessage {
						typ := msgTypeTermData
						if data[0] == 1 {
							typ = msgTypeFile
						}
						dev.WriteMsg(typ, append([]byte(msg.sid), data[1:]...))
					} else {
						typ := jsoniter.Get(data, "type").ToString()

						switch typ {
						case "winsize":
							b := [32 + 4]byte{}

							copy(b[:], msg.sid)

							cols := jsoniter.Get(data, "cols").ToUint()
							rows := jsoniter.Get(data, "rows").ToUint()

							binary.BigEndian.PutUint16(b[32:], uint16(cols))
							binary.BigEndian.PutUint16(b[34:], uint16(rows))

							dev.WriteMsg(msgTypeWinsize, b[:])

						case "ack":
							b := [32 + 2]byte{}
							copy(b[:], msg.sid)

							ack := jsoniter.Get(data, "ack").ToUint()
							binary.BigEndian.PutUint16(b[32:], uint16(ack))
							dev.WriteMsg(msgTypeAck, b[:])

						case "fileInfo":
							size := jsoniter.Get(data, "size").ToUint32()
							name := jsoniter.Get(data, "name").ToString()

							b := make([]byte, 32+1+4+len(name))
							copy(b[:], msg.sid)
							b[32] = msgTypeFileInfo
							binary.BigEndian.PutUint32(b[33:], size)
							copy(b[37:], name)
							dev.WriteMsg(msgTypeFile, b[:])

						case "fileCanceled":
							b := [33]byte{}
							copy(b[:], msg.sid)
							b[32] = msgTypeFileAbort
							dev.WriteMsg(msgTypeFile, b[:])
						}
					}
				}
			} else {
				log.Error().Msg("Not found sid: " + msg.sid)
			}

		case devid := <-br.heartbeat:
			if dev, ok := br.getDevice(devid); ok {
				dev.WriteMsg(msgTypeHeartbeat, []byte{})
			}
		case req := <-br.cmdReq:
			if dev, ok := br.getDevice(req.devid); ok {
				dev.WriteMsg(msgTypeCmd, req.data)
			}

		case data := <-br.cmdResp:
			handleCmdResp(data)

		case req := <-br.httpReq:
			if dev, ok := br.getDevice(req.devid); ok {
				dev.WriteMsg(msgTypeHttp, req.data)
			}

		case resp := <-br.httpResp:
			handleHttpProxyResp(resp)
		}
	}
}
