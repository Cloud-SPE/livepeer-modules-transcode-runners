package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net"
	"time"

	"github.com/sirupsen/logrus"
	flvtag "github.com/yutopp/go-flv/tag"
	rtmp "github.com/yutopp/go-rtmp"
	rtmpmsg "github.com/yutopp/go-rtmp/message"
)

type sharedIngestServer struct {
	listener net.Listener
	server   *rtmp.Server
}

func startSharedIngest(store *sessionStore, metrics *runnerMetrics, addr string) (*sharedIngestServer, error) {
	if addr == "" {
		return nil, nil
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	srv := rtmp.NewServer(&rtmp.ServerConfig{
		OnConnect: func(conn net.Conn) (io.ReadWriteCloser, *rtmp.ConnConfig) {
			logger := logrus.New()
			logger.SetOutput(io.Discard)
			return conn, &rtmp.ConnConfig{
				Handler: &gatewayIngestHandler{
					store:   store,
					metrics: metrics,
				},
				ControlState: rtmp.StreamControlStateConfig{
					DefaultBandwidthWindowSize: 6 * 1024 * 1024 / 8,
				},
				Logger: logger,
			}
		},
	})
	g := &sharedIngestServer{listener: l, server: srv}
	go func() {
		if err := srv.Serve(l); err != nil && err != rtmp.ErrClosed {
			log.Printf("shared ingest server stopped: %v", err)
		}
	}()
	return g, nil
}

func (s *sharedIngestServer) Close() error {
	if s == nil {
		return nil
	}
	if s.server != nil {
		_ = s.server.Close()
	}
	if s.listener != nil {
		return s.listener.Close()
	}
	return nil
}

type gatewayIngestHandler struct {
	rtmp.DefaultHandler
	store   *sessionStore
	metrics *runnerMetrics

	session *sessionRuntime
	client  *rtmp.ClientConn
	stream  *rtmp.Stream
}

func (h *gatewayIngestHandler) OnPublish(_ *rtmp.StreamContext, _ uint32, cmd *rtmpmsg.NetStreamPublish) error {
	rt := h.store.byStreamKey(cmd.PublishingName)
	if rt == nil || rt.rec.Mode != modeGatewayIngest {
		if h.metrics != nil {
			h.metrics.ingestAuthenticationReject.Add(1)
		}
		return fmt.Errorf("unknown ingest stream")
	}
	client, stream, err := dialInternalPublish(rt)
	if err != nil {
		return err
	}
	h.session = rt
	h.client = client
	h.stream = stream
	rt.notePublisherAccepted()
	return nil
}

func (h *gatewayIngestHandler) OnPlay(_ *rtmp.StreamContext, _ uint32, _ *rtmpmsg.NetStreamPlay) error {
	if h.metrics != nil {
		h.metrics.ingestAuthenticationReject.Add(1)
	}
	return fmt.Errorf("play not supported")
}

func (h *gatewayIngestHandler) OnSetDataFrame(timestamp uint32, data *rtmpmsg.NetStreamSetDataFrame) error {
	if h.stream == nil || h.session == nil {
		return fmt.Errorf("publish not initialized")
	}
	h.session.noteIngestPacket(time.Now().UTC())
	return h.stream.WriteDataMessage(8, timestamp, "@setDataFrame", &rtmpmsg.NetStreamSetDataFrame{Payload: data.Payload})
}

func (h *gatewayIngestHandler) OnAudio(timestamp uint32, payload io.Reader) error {
	if h.stream == nil || h.session == nil {
		return fmt.Errorf("publish not initialized")
	}
	var audio flvtag.AudioData
	if err := flvtag.DecodeAudioData(payload, &audio); err != nil {
		return err
	}
	buf := new(bytes.Buffer)
	if err := flvtag.EncodeAudioData(buf, &audio); err != nil {
		return err
	}
	h.session.noteIngestPacket(time.Now().UTC())
	return h.stream.Write(5, timestamp, &rtmpmsg.AudioMessage{Payload: buf})
}

func (h *gatewayIngestHandler) OnVideo(timestamp uint32, payload io.Reader) error {
	if h.stream == nil || h.session == nil {
		return fmt.Errorf("publish not initialized")
	}
	var video flvtag.VideoData
	if err := flvtag.DecodeVideoData(payload, &video); err != nil {
		return err
	}
	buf := new(bytes.Buffer)
	if err := flvtag.EncodeVideoData(buf, &video); err != nil {
		return err
	}
	h.session.noteIngestPacket(time.Now().UTC())
	return h.stream.Write(6, timestamp, &rtmpmsg.VideoMessage{Payload: buf})
}

func (h *gatewayIngestHandler) OnClose() {
	if h.stream != nil {
		_ = h.stream.Close()
	}
	if h.client != nil {
		_ = h.client.Close()
	}
	if h.session != nil {
		h.session.notePublisherDisconnected("publisher_disconnected")
		h.session.event("session.publish_stopped", h.session.lastUsageTotal.Load(), 0, "publisher_disconnected", nil)
	}
}

func dialInternalPublish(rt *sessionRuntime) (*rtmp.ClientConn, *rtmp.Stream, error) {
	addr := net.JoinHostPort("127.0.0.1", itoa(rt.rec.RTMPPort))
	client, err := rtmp.Dial("rtmp", addr, &rtmp.ConnConfig{})
	if err != nil {
		return nil, nil, err
	}
	connect := &rtmpmsg.NetConnectionConnect{
		Command: rtmpmsg.NetConnectionConnectCommand{
			App:           "live",
			TCURL:         "rtmp://" + addr + "/live",
			FlashVer:      "live-runner-shared-ingest",
			Capabilities:  15,
			AudioCodecs:   4071,
			VideoCodecs:   252,
			VideoFunction: 1,
		},
	}
	if err := client.Connect(connect); err != nil {
		_ = client.Close()
		return nil, nil, err
	}
	stream, err := client.CreateStream(nil, 128)
	if err != nil {
		_ = client.Close()
		return nil, nil, err
	}
	if err := stream.Publish(&rtmpmsg.NetStreamPublish{
		PublishingName: rt.rec.StreamKey,
		PublishingType: "live",
	}); err != nil {
		_ = client.Close()
		return nil, nil, err
	}
	return client, stream, nil
}
