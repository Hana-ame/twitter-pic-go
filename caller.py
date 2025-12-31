import os
# import sys
import threading
import socket

def call(username):
    os.system(f"py get_meta_data.py {username}")


def handle_client(client_socket, client_address):
    """处理单个客户端连接的线程函数"""
    print(f"[+] 客户端 {client_address} 已连接")
    try:
        # 接收客户端发送的数据
        data = client_socket.recv(1024)  # 一次最多接收1024字节
        if not data:
            print(f"[-] 客户端 {client_address} 发送了空数据")
            return
            
        received_str = data.decode('utf-8').strip() # 解码并去除首尾空白字符
        print(f"[*] 收到来自 {client_address} 的字符串: {repr(received_str)}")
        
        # 使用接收到的字符串执行调用
        result = call(received_str)
        
        # (可选)将调用结果发送回客户端
        # response = str(result).encode('utf-8')
        # client_socket.sendall(response)
        
    except UnicodeDecodeError:
        print(f"[-] 来自 {client_address} 的数据无法解码为 UTF-8 字符串")
    except Exception as e:
        print(f"[-] 处理客户端 {client_address} 请求时发生错误: {e}")
    finally:
        # 关闭当前客户端连接
        client_socket.close()
        print(f"[-] 与客户端 {client_address} 的连接已关闭")



def start_tcp_listener(host='127.25.9.19', port=8080):
    """启动TCP监听服务器"""
    # 创建TCP socket[1,2,3](@ref)
    server_socket = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    
    # 设置地址重用选项，避免重启服务器时遇到“Address already in use”错误
    server_socket.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    
    try:
        # 绑定IP地址和端口[1,2,3](@ref)
        server_socket.bind((host, port))
        # 开始监听，设置最大等待连接数[1,2,3](@ref)
        server_socket.listen(5)
        print(f"[*] TCP 监听器已启动在 {host}:{port}")
        print("[*] 等待客户端连接...")
        
        while True:
            # 接受客户端连接[1,2,3](@ref)
            client_sock, client_addr = server_socket.accept()
            # 为每个新连接创建一个新线程进行处理[6,8](@ref)
            client_thread = threading.Thread(target=handle_client, args=(client_sock, client_addr))
            client_thread.daemon = True # 设置为守护线程，主程序退出时自动结束
            client_thread.start()
            print(f"[*] 活跃连接数: {threading.active_count() - 1}")
            
    except KeyboardInterrupt:
        print("\n[!] 收到中断信号，服务器关闭中...")
    except Exception as e:
        print(f"[!] 服务器运行出错: {e}")
    finally:
        # 确保服务器Socket被关闭[1,2](@ref)
        server_socket.close()
        print("[-] 服务器已关闭")
        
            
if __name__ == '__main__':
    # 创建一个tcp listener, 每当发生链接时,接收一个字符串,字符串用来执行call(s)
    start_tcp_listener()