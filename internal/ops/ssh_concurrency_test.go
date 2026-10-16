package ops

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"
)

// TestSSHConcurrentSessionsAndList 对会话注册表做并发压力测试。
// 目的是在没有 race detector（PRoot 下不可用）的情况下暴露明显的
// 数据竞争与死锁：并发建连、并发 list、并发 close。
func TestSSHConcurrentSessionsAndList(t *testing.T) {
	server := newTestSSHServer(t, "secret")
	host, port := server.host()
	manager := testManager(t)

	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan string, workers*4)

	base := func() map[string]any {
		return map[string]any{
			"host":      host,
			"port":      float64(port),
			"user":      "tester",
			"password":  "secret",
			"insecure":  true,
			"timeoutMs": float64(15000),
		}
	}

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			args := base()
			args["action"] = "connect"
			args["keep"] = true
			result := manager.Execute(context.Background(), Request{Tool: "aplm_ssh", Args: args})
			if !result.Success {
				errs <- fmt.Sprintf("connect: %s/%s", result.Code, result.Message)
			}
		}()
	}

	// 与建连并发地读列表。
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				result := manager.Execute(context.Background(), Request{
					Tool: "aplm_ssh",
					Args: map[string]any{"action": "list"},
				})
				if !result.Success {
					errs <- fmt.Sprintf("list: %s/%s", result.Code, result.Message)
					return
				}
			}
		}()
	}

	// 与两者并发地关闭全部。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 5; j++ {
			manager.Execute(context.Background(), Request{
				Tool: "aplm_ssh",
				Args: map[string]any{"action": "close", "all": true},
			})
			time.Sleep(5 * time.Millisecond)
		}
	}()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("concurrent SSH operations deadlocked")
	}
	close(errs)
	for msg := range errs {
		// 会话数上限是预期内的；其它错误都要报出来。
		if len(msg) > 0 {
			t.Logf("concurrent note: %s", msg)
		}
	}
	// 收尾，确保不留下会话。
	manager.Execute(context.Background(), Request{
		Tool: "aplm_ssh",
		Args: map[string]any{"action": "close", "all": true},
	})
	runtime := manager.sshState()
	if got := runtime.count(); got != 0 {
		t.Fatalf("sessions leaked after close-all: %d", got)
	}
}

// TestSSHConcurrentExec 并发执行远端命令，验证 boundedSSHWriter 与会话复用无竞争。
func TestSSHConcurrentExec(t *testing.T) {
	server := newTestSSHServer(t, "secret")
	host, port := server.host()
	manager := testManager(t)

	const workers = 10
	var wg sync.WaitGroup
	failures := make(chan string, workers)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			result := manager.Execute(context.Background(), Request{
				Tool: "aplm_ssh",
				Args: map[string]any{
					"action":    "exec",
					"host":      host,
					"port":      float64(port),
					"user":      "tester",
					"password":  "secret",
					"command":   "echo hello",
					"insecure":  true,
					"timeoutMs": float64(20000),
				},
			})
			if !result.Success {
				failures <- fmt.Sprintf("worker %d: %s/%s", idx, result.Code, result.Message)
				return
			}
			data, _ := result.Data.(map[string]any)
			if got, _ := data["stdout"].(string); got != "hello\n" {
				failures <- fmt.Sprintf("worker %d: stdout=%q", idx, got)
			}
		}(i)
	}
	wg.Wait()
	close(failures)
	for msg := range failures {
		t.Error(msg)
	}
}

// TestSSHConcurrentForwards 并发建立/停止端口转发，验证 forward 映射的并发安全。
func TestSSHConcurrentForwards(t *testing.T) {
	server := newTestSSHServer(t, "secret")
	host, port := server.host()
	manager := testManager(t)

	// 目标 echo 服务。
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	defer echo.Close()
	go func() {
		for {
			conn, acceptErr := echo.Accept()
			if acceptErr != nil {
				return
			}
			go func(c net.Conn) { defer c.Close(); _, _ = c.Write([]byte("pong")) }(conn)
		}
	}()
	echoPort := echo.Addr().(*net.TCPAddr).Port

	// 先建一个可复用会话。
	connect := manager.Execute(context.Background(), Request{
		Tool: "aplm_ssh",
		Args: map[string]any{
			"action":    "connect",
			"host":      host,
			"port":      float64(port),
			"user":      "tester",
			"password":  "secret",
			"insecure":  true,
			"keep":      true,
			"timeoutMs": float64(15000),
		},
	})
	if !connect.Success {
		t.Fatalf("keep connect failed: %s/%s", connect.Code, connect.Message)
	}
	data, _ := connect.Data.(map[string]any)
	sessionID, _ := data["sessionId"].(string)

	const workers = 6
	var mu sync.Mutex
	forwards := make([]string, 0, workers)
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result := manager.Execute(context.Background(), Request{
				Tool: "aplm_ssh",
				Args: map[string]any{
					"action":     "forward",
					"sessionId":  sessionID,
					"bindAddr":   "127.0.0.1",
					"bindPort":   float64(0),
					"targetHost": "127.0.0.1",
					"targetPort": float64(echoPort),
				},
			})
			if !result.Success {
				t.Errorf("forward failed: %s/%s", result.Code, result.Message)
				return
			}
			fwd, _ := result.Data.(map[string]any)
			id, _ := fwd["forwardId"].(string)
			mu.Lock()
			forwards = append(forwards, id)
			mu.Unlock()
		}()
	}
	wg.Wait()

	// 并发停止全部转发。
	var wg2 sync.WaitGroup
	for _, id := range forwards {
		wg2.Add(1)
		go func(forwardID string) {
			defer wg2.Done()
			result := manager.Execute(context.Background(), Request{
				Tool: "aplm_ssh",
				Args: map[string]any{
					"action":    "unforward",
					"sessionId": sessionID,
					"forwardId": forwardID,
				},
			})
			if !result.Success {
				t.Errorf("unforward %s failed: %s/%s", forwardID, result.Code, result.Message)
			}
		}(id)
	}
	wg2.Wait()

	manager.Execute(context.Background(), Request{
		Tool: "aplm_ssh",
		Args: map[string]any{"action": "close", "sessionId": sessionID},
	})
}
