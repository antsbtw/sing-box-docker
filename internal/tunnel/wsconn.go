package tunnel

// 把 WebSocket 包装成 net.Conn。
//
// 契约 §2.2：隧道 WS 上跑的是裸 SSH 字节流，agent 要把它当一条 TCP 连接
// 交给 x/crypto/ssh 的服务端。ssh.NewServerConn 需要 net.Conn，
// 而 gorilla/websocket 给的是帧接口，故包一层。

import (
	"io"
	"net"
	"time"

	"github.com/gorilla/websocket"
)

// wsConn 让 *websocket.Conn 满足 net.Conn。
//
// ⚠️ 帧边界与字节流边界无关：一个 SSH 报文可能跨多帧，
// 一帧也可能含多个报文。Read 按字节流语义拼接，不保留帧边界。
type wsConn struct {
	ws     *websocket.Conn
	reader io.Reader // 当前帧的读取器，读完换下一帧
}

func newWSConn(ws *websocket.Conn) net.Conn {
	return &wsConn{ws: ws}
}

func (c *wsConn) Read(p []byte) (int, error) {
	for {
		if c.reader == nil {
			typ, r, err := c.ws.NextReader()
			if err != nil {
				return 0, err
			}
			// 契约 §1.2：只用二进制帧。文本帧后端也当二进制搬，
			// 但这里收到就当数据处理，不特殊对待。
			_ = typ
			c.reader = r
		}

		n, err := c.reader.Read(p)
		if err == io.EOF {
			// 当前帧读完，换下一帧再读 —— 对调用方来说是连续字节流
			c.reader = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
}

func (c *wsConn) Write(p []byte) (int, error) {
	// 每次 Write 一帧。SSH 的写入本身是分组的，不需要额外缓冲。
	if err := c.ws.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *wsConn) Close() error                       { return c.ws.Close() }
func (c *wsConn) LocalAddr() net.Addr                { return c.ws.LocalAddr() }
func (c *wsConn) RemoteAddr() net.Addr               { return c.ws.RemoteAddr() }
func (c *wsConn) SetDeadline(t time.Time) error      { return c.ws.UnderlyingConn().SetDeadline(t) }
func (c *wsConn) SetReadDeadline(t time.Time) error  { return c.ws.SetReadDeadline(t) }
func (c *wsConn) SetWriteDeadline(t time.Time) error { return c.ws.SetWriteDeadline(t) }
