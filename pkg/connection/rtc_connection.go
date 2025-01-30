package connection

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/realtime-ai/realtime-ai/pkg/elements"
	"github.com/realtime-ai/realtime-ai/pkg/pipeline"
)

type RTCConnection interface {
	// PeerID 返回此连接对应的唯一标识
	PeerID() string

	// PeerConnection 返回底层的 *webrtc.PeerConnection
	PeerConnection() *webrtc.PeerConnection

	// AddDataChannel 记录/管理新的 DataChannel（本地或远端创建）
	DataChannel() *webrtc.DataChannel

	// RemoteAudioTrack 返回远端音频流
	RemoteAudioTrack() *webrtc.TrackRemote

	// LocalAudioTrack 返回本地音频流
	LocalAudioTrack() *webrtc.TrackLocalStaticSample

	// Start 开始连接
	Start(ctx context.Context) error

	// Close 关闭底层的 PeerConnection (并执行相应清理)
	Close() error
}

type rtcConnectionImpl struct {
	peerID string

	// 底层 Pion WebRTC 对象
	pc *webrtc.PeerConnection

	// DataChannel 管理
	dataChannel *webrtc.DataChannel

	// 远端音频流
	remoteAudioTrack *webrtc.TrackRemote

	// 本地音频流
	localAudioTrack *webrtc.TrackLocalStaticSample

	// for openai
	openaiElement *elements.OpenAIRealtimeAPIElement

	// for gemini
	geminiElement *elements.GeminiElement

	// for webrtc
	webrtcSinkElement      *elements.WebRTCSinkElement
	opusDecodeElement      *elements.OpusDecodeElement
	inAudioResampleElement *elements.AudioResampleElement

	pipeline *pipeline.Pipeline
}

var _ RTCConnection = (*rtcConnectionImpl)(nil)

func NewRTCConnection(peerID string, pc *webrtc.PeerConnection) RTCConnection {

	return &rtcConnectionImpl{
		peerID: peerID,
		pc:     pc,
	}
}

func (c *rtcConnectionImpl) PeerID() string {
	return c.peerID
}

func (c *rtcConnectionImpl) PeerConnection() *webrtc.PeerConnection {
	return c.pc
}

func (c *rtcConnectionImpl) DataChannel() *webrtc.DataChannel {
	return c.dataChannel
}

func (c *rtcConnectionImpl) RemoteAudioTrack() *webrtc.TrackRemote {
	return c.remoteAudioTrack
}

func (c *rtcConnectionImpl) LocalAudioTrack() *webrtc.TrackLocalStaticSample {
	return c.localAudioTrack
}

func (c *rtcConnectionImpl) Start(ctx context.Context) error {

	c.pc.OnDataChannel(func(d *webrtc.DataChannel) {
		log.Printf("DataChannel created: %s", d.Label())

		c.dataChannel = d

		go c.readDataChannel(ctx)
	})

	c.pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		log.Printf("OnTrack: %v, codec: %v", track.ID(), track.Codec().MimeType)
		if track.Kind() == webrtc.RTPCodecTypeAudio {
			c.remoteAudioTrack = track
			go c.readRemoteAudio(ctx)
		}
	})

	audioTrack, audioTrackErr := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}, "audio", "pion")
	if audioTrackErr != nil {
		log.Println("create local audio track error:", audioTrackErr)
		return audioTrackErr
	}
	c.localAudioTrack = audioTrack

	c.pc.AddTransceiverFromTrack(c.localAudioTrack, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionSendrecv,
	})

	webrtcSinkElement := elements.NewWebRTCSinkElement(c.localAudioTrack)
	geminiElement := elements.NewGeminiElement()
	openaiElement := elements.NewOpenAIRealtimeAPIElement()

	opusDecodeElement := elements.NewOpusDecodeElement(48000, 1)
	inAudioResampleElement := elements.NewAudioResampleElement(48000, 16000, 1, 1)

	var elements []pipeline.Element

	// 如果使用 OpenAI Realtime API，则需要添加 OpenAI Realtime API Element
	if os.Getenv("USING_OPENAI_REALTIM_API") == "true" {
		elements = []pipeline.Element{
			opusDecodeElement,
			inAudioResampleElement,
			openaiElement,
			webrtcSinkElement,
		}
	} else {
		// 如果使用 Gemini，则需要添加 Gemini Element
		elements = []pipeline.Element{
			opusDecodeElement,
			inAudioResampleElement,
			geminiElement,
			webrtcSinkElement,
		}
	}

	bus := pipeline.NewEventBus()
	pipeline := pipeline.NewPipeline("rtc_connection", bus)
	pipeline.AddElements(elements)

	pipeline.Link(opusDecodeElement, inAudioResampleElement)
	if os.Getenv("USING_OPENAI_REALTIM_API") == "true" {
		pipeline.Link(inAudioResampleElement, openaiElement)
		pipeline.Link(openaiElement, webrtcSinkElement)
	} else {
		pipeline.Link(inAudioResampleElement, geminiElement)
		pipeline.Link(geminiElement, webrtcSinkElement)
	}

	c.webrtcSinkElement = webrtcSinkElement
	c.opusDecodeElement = opusDecodeElement
	c.inAudioResampleElement = inAudioResampleElement
	c.geminiElement = geminiElement
	c.openaiElement = openaiElement

	c.pipeline = pipeline

	return pipeline.Start(ctx)
}

func (c *rtcConnectionImpl) Close() error {

	return c.pipeline.Stop()
}

func (c *rtcConnectionImpl) readRemoteAudio(ctx context.Context) {

	for {
		select {
		case <-ctx.Done():
			return
		default:
			rtpPacket, _, err := c.remoteAudioTrack.ReadRTP()
			if err != nil {
				log.Println("read RTP error:", err)
				continue
			}

			// 将拿到的 payload 投递给 pipeline 的“输入 element”
			msg := pipeline.PipelineMessage{
				Type: pipeline.MsgTypeAudio,
				AudioData: &pipeline.AudioData{
					Data:       rtpPacket.Payload,
					SampleRate: 48000,
					Channels:   1,
					MediaType:  "audio/x-opus",
					Codec:      "opus",
					Timestamp:  time.Now(),
				},
			}

			c.opusDecodeElement.In() <- msg
		}
	}
}

func (c *rtcConnectionImpl) readDataChannel(ctx context.Context) {

	defer c.dataChannel.Close()

	c.dataChannel.OnMessage(func(msg webrtc.DataChannelMessage) {

		// TODO: 暂时不支持文本消息
		// message := msg.Data
		// c.geminiElement.In() <- pipeline.PipelineMessage{
		// 	Type: pipeline.MsgTypeText,
		// 	TextData: &pipeline.TextData{
		// 		Data:      string(message),
		// 		TextType:  "text",
		// 		Timestamp: time.Now(),
		// 	},
		// }
	})

	<-ctx.Done()
}

// =====================================================
