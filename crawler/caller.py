import os
import shlex
import socket
import threading
import queue

# 统一使用主力抓取脚本 get2.py（带 CT0 / 源 IP 绑定与完整的 users 统计列更新）
FETCHER = "get2.py"

# caller.py 的 TCP listener 绑在非 loopback 地址上且能触发命令执行。
# 设置 CALLER_TOKEN 后要求客户端第一行发送 "token <CALLER_TOKEN>"，
# 未设置时保持旧行为（向后兼容，不破坏现有部署）。
CALLER_TOKEN = os.getenv("CALLER_TOKEN", "")

# 有界任务队列与去重集合：防止突发请求无上限派生 Python 抓取进程耗尽 528MB 机器内存
MAX_QUEUE_SIZE = int(os.getenv("CALLER_QUEUE_SIZE", "100"))
WORKER_COUNT = int(os.getenv("CALLER_WORKERS", "2"))

task_queue: "queue.Queue[str]" = queue.Queue(maxsize=MAX_QUEUE_SIZE)
pending_lock = threading.Lock()
pending_users: set[str] = set()
workers_started = False


def call(username: str) -> int:
    # 注意：username 来自网络（Go 服务器转发），必须 quote，
    # 否则注入 `;`、`$()` 等字符会直接执行 shell 命令（RCE）。
    return os.system(f"timeout 300s ~/twitter/venv/bin/python3 {FETCHER} {shlex.quote(username)}")


def _worker_loop(worker_id: int):
    while True:
        username = task_queue.get()
        try:
            print(f"[Worker-{worker_id}] 开始抓取: {username}")
            call(username)
            print(f"[Worker-{worker_id}] 完成抓取: {username}")
        except Exception as e:
            print(f"[Worker-{worker_id}] 抓取 {username} 异常: {e}")
        finally:
            with pending_lock:
                pending_users.discard(username.lower())
            task_queue.task_done()


def ensure_workers_started():
    global workers_started
    with pending_lock:
        if workers_started:
            return
        workers_started = True
        for i in range(max(1, WORKER_COUNT)):
            t = threading.Thread(target=_worker_loop, args=(i + 1,), daemon=True)
            t.start()


def enqueue_username(username: str) -> str:
    """将 username 加入有界抓取队列，自动剔除正在队列或执行中的重复 username。"""
    ensure_workers_started()
    key = username.strip().lower()
    if not key:
        return "empty"
    with pending_lock:
        if key in pending_users:
            print(f"[*] 用户 {username} 已在抓取队列或执行中，跳过重复入队")
            return "duplicate"
        try:
            task_queue.put_nowait(username.strip())
            pending_users.add(key)
            print(f"[+] 用户 {username} 已入队（当前队列深度: {task_queue.qsize()}/{MAX_QUEUE_SIZE}）")
            return "queued"
        except queue.Full:
            print(f"[!] 抓取队列已满 ({MAX_QUEUE_SIZE})，丢弃请求: {username}")
            return "full"


def token_ok(first_line: str) -> bool:
    """校验可选的共享令牌。CALLER_TOKEN 为空时不启用鉴权。

    两侧都转小写再比较：只 lower 输入侧会让带大写字母的 token 永远校验失败。
    """
    if not CALLER_TOKEN:
        return True
    return first_line.strip().lower() == f"token {CALLER_TOKEN}".lower()


def handle_client(client_socket, client_address):
    """处理单个客户端连接：校验参数后将任务放入有界队列立即返回，不在连接线程内同步跑爬虫。"""
    print(f"[+] 客户端 {client_address} 已连接")
    try:
        data = client_socket.recv(1024)
        if not data:
            print(f"[-] 客户端 {client_address} 发送了空数据")
            return

        received_str = data.decode('utf-8').strip()
        print(f"[*] 收到来自 {client_address} 的字符串: {repr(received_str)}")

        lines = received_str.splitlines()
        username = lines[-1].strip() if lines else ""
        if CALLER_TOKEN:
            if len(lines) < 2 or not token_ok(lines[0]):
                print(f"[-] 客户端 {client_address} 令牌校验失败，拒绝执行")
                return
        if not username:
            print(f"[-] 客户端 {client_address} 未提供 username")
            return

        enqueue_username(username)

    except UnicodeDecodeError:
        print(f"[-] 来自 {client_address} 的数据无法解码为 UTF-8 字符串")
    except Exception as e:
        print(f"[-] 处理客户端 {client_address} 请求时发生错误: {e}")
    finally:
        client_socket.close()
        print(f"[-] 与客户端 {client_address} 的连接已关闭")


def start_tcp_listener(host='127.25.9.19', port=8080):
    """启动TCP监听服务器"""
    ensure_workers_started()
    server_socket = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    server_socket.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)

    try:
        server_socket.bind((host, port))
        server_socket.listen(16)
        print(f"[*] TCP 监听器已启动在 {host}:{port} (workers={WORKER_COUNT}, max_queue={MAX_QUEUE_SIZE})")
        print("[*] 等待客户端连接...")

        while True:
            client_sock, client_addr = server_socket.accept()
            client_thread = threading.Thread(target=handle_client, args=(client_sock, client_addr))
            client_thread.daemon = True
            client_thread.start()

    except KeyboardInterrupt:
        print("\n[!] 收到中断信号，服务器关闭中...")
    except Exception as e:
        print(f"[!] 服务器运行出错: {e}")
    finally:
        server_socket.close()
        print("[-] 服务器已关闭")


if __name__ == '__main__':
    start_tcp_listener()
