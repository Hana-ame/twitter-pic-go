import os
import shlex
import time

# 文件路径配置
COMMANDS_FILE = "commands.txt"       # 存放待执行指令的文件
PENDING_INPUT_FILE = "pending.txt"  # 存放原始输入的文件

# 抓取脚本。build.sh 上传的是 get_meta_data.py，这里必须与之一致，
# 否则队列为空跑不起来（历史上这里写的是不存在的 get2.py）。
FETCHER = "get_meta_data.py"
FETCHER_BIN = "~/twitter/venv/bin/python3"

def extract_username(raw_text):
    """从输入里提取 Twitter username。

    支持两种输入：
      * 完整 URL：https://x.example.com/lunara_fawn[/?...]
      * 裸 username：lunara_fawn

    只返回路径的最后一段。URL 没有路径段时返回 None —— 否则 rstrip('/') 之后
    split('/')[-1] 会拿到主机名（如 https://x.example.com/ -> "x.example.com"），
    产生一条注定失败的抓取任务。
    """
    line = raw_text.strip().rstrip('/')
    if not line:
        return None

    if '://' in line:
        # 剥掉 scheme
        line = line.split('://', 1)[1]
        # 剥掉 host[:port]；没有路径段说明 URL 里根本没有 username
        if '/' not in line:
            return None
        line = line.split('/', 1)[1]
        # 去掉 query / fragment
        line = line.split('?', 1)[0].split('#', 1)[0]

    username = line.split('/')[-1].strip()
    if not username:
        return None
    # username 里出现路径分隔符说明解析出了问题，直接放弃
    if '/' in username or '\\' in username:
        return None
    return username


def translate_logic(raw_text):
    """
    将 URL 转换为系统指令
    输入: https://x.nmbyd3.top/lunara_fawn
    输出: [ ! -f lunara_fawn.json.gz ] && timeout 300s ~/twitter/venv/bin/python3 get_meta_data.py lunara_fawn
    """
    username = extract_username(raw_text)
    if not username:
        return None

    # username 来自外部输入，必须 shlex.quote：否则 `;`、`$()`、反引号、空格等
    # 会直接改变 shell 语义。命令稍后由 execute_and_pop_command() 经 os.system()
    # 执行，属于 RCE 面。caller.py 对同类输入做了同样处理。
    target_file = f"{username}.json.gz"

    # 这里使用 Shell 的逻辑判断：
    # [ ! -f 文件名 ] 表示“如果文件不存在”
    # && 表示“则执行后面的命令”
    # 注意：这里假设你的运行环境是 Linux/macOS 或 Windows 的 Bash 环境
    translated_command = (
        f"[ ! -f {shlex.quote(target_file)} ] && timeout 300s "
        f"{FETCHER_BIN} {FETCHER} {shlex.quote(username)}"
    )

    return translated_command

# --- 以下是配合你之前的程序逻辑的建议修正 ---

def process_pending_file():
    """读取 pending 文件，翻译并追加到 commands 文件末尾"""
    if not os.path.exists(PENDING_INPUT_FILE) or os.path.getsize(PENDING_INPUT_FILE) == 0:
        return

    print(f"发现新内容，正在处理...")
    
    translated_list = []
    with open(PENDING_INPUT_FILE, 'r', encoding='utf-8') as f:
        lines = f.readlines()

    for line in lines:
        cmd = translate_logic(line)
        if cmd:
            translated_list.append(cmd)

    if translated_list:
        with open(COMMANDS_FILE, 'a', encoding='utf-8') as f:
            for cmd in translated_list:
                f.write(cmd + "\n")
        print(f"成功追加 {len(translated_list)} 条新指令。")

    # 清空 pending 文件
    open(PENDING_INPUT_FILE, 'w').close()
    
def execute_and_pop_command():
    """读取第一行指令，执行它，并从文件中删除该行"""
    if not os.path.exists(COMMANDS_FILE) or os.path.getsize(COMMANDS_FILE) == 0:
        print("指令队列为空，跳过本次执行。")
        return

    # 读取所有行
    with open(COMMANDS_FILE, 'r', encoding='utf-8') as f:
        lines = f.readlines()

    if not lines:
        return

    # 提取第一行并移除
    current_command = lines[0].strip()
    remaining_commands = lines[1:]

    if current_command:
        print(f"正在执行指令: {current_command}")
        # 执行系统指令
        os.system(current_command)
    
    # 将剩余的行写回文件（相当于删除了第一行）
    with open(COMMANDS_FILE, 'w', encoding='utf-8') as f:
        f.writelines(remaining_commands)

def main():
    print("程序已启动，每 15 分钟检查一次...")
    
    # 如果文件不存在则创建
    for f in [COMMANDS_FILE, PENDING_INPUT_FILE]:
        if not os.path.exists(f):
            with open(f, 'w') as _: pass

    while True:
        # 1. 先处理待翻译的文件
        process_pending_file()
        
        # 2. 执行并弹出第一条指令
        execute_and_pop_command()
        
        # 3. 等待 15 分钟 (15 * 60 秒)
        print("进入休眠，等待下一次循环...")
        time.sleep(15 * 60)

if __name__ == "__main__":
    try:
        main()
    except KeyboardInterrupt:
        print("\n程序已手动停止。")