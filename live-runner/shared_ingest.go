package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"time"

	"github.com/sirupsen/logrus"
	flv "github.com/yutopp/go-flv"
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
	writer  io.WriteCloser
	encoder *flv.Encoder
}

func (h *gatewayIngestHandler) OnPublish(_ *rtmp.StreamContext, _ uint32, cmd *rtmpmsg.NetStreamPublish) error {
	rt := h.store.byStreamKey(cmd.PublishingName)
	if rt == nil || rt.rec.Mode != modeGatewayIngest {
		if h.metrics != nil {
			h.metrics.ingestAuthenticationReject.Add(1)
		}
		return fmt.Errorf("unknown ingest stream")
	}
	writer, encoder, err := openIngestPipe(rt)
	if err != nil {
		return err
	}
	h.session = rt
	h.writer = writer
	h.encoder = encoder
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
	if h.session == nil {
		return fmt.Errorf("publish not initialized")
	}
	h.session.noteIngestPacket(time.Now().UTC())
	_ = timestamp
	_ = data
	return nil
}

func (h *gatewayIngestHandler) OnAudio(timestamp uint32, payload io.Reader) error {
	if h.encoder == nil || h.session == nil {
		return fmt.Errorf("publish not initialized")
	}
	var audio flvtag.AudioData
	if err := flvtag.DecodeAudioData(payload, &audio); err != nil {
		return err
	}
	h.session.noteIngestPacket(time.Now().UTC())
	return h.encoder.Encode(&flvtag.FlvTag{
		TagType:   flvtag.TagTypeAudio,
		Timestamp: timestamp,
		Data:      &audio,
	})
}

func (h *gatewayIngestHandler) OnVideo(timestamp uint32, payload io.Reader) error {
	if h.encoder == nil || h.session == nil {
		return fmt.Errorf("publish not initialized")
	}
	var video flvtag.VideoData
	if err := flvtag.DecodeVideoData(payload, &video); err != nil {
		return err
	}
	h.session.noteIngestPacket(time.Now().UTC())
	return h.encoder.Encode(&flvtag.FlvTag{
		TagType:   flvtag.TagTypeVideo,
		Timestamp: timestamp,
		Data:      &video,
	})
}

func (h *gatewayIngestHandler) OnClose() {
	if h.writer != nil {
		_ = h.writer.Close()
	}
	if h.session != nil {
		h.session.notePublisherDisconnected("publisher_disconnected")
		h.session.event("session.publish_stopped", h.session.lastUsageTotal.Load(), 0, "publisher_disconnected", nil)
	}
}

func openIngestPipe(rt *sessionRuntime) (io.WriteCloser, *flv.Encoder, error) {
	f, err := os.OpenFile(rt.rec.IngestPipePath, os.O_WRONLY, os.ModeNamedPipe)
	if err != nil {
		return nil, nil, err
	}
	enc, err := flv.NewEncoder(f, flv.FlagsAudio|flv.FlagsVideo)
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	return f, enc, nil
}
