import os
import time

# 文件路径配置
COMMANDS_FILE = "commands.txt"       # 存放待执行指令的文件
PENDING_INPUT_FILE = "pending.txt"  # 存放原始输入的文件

def translate_logic(raw_text):
    """
    将 URL 转换为系统指令
    输入: https://x.nmbyd3.top/lunara_fawn
    输出: [ ! -f lunara_fawn.json.gz ] && timeout 300s py get_meta_data.py lunara_fawn
    """
    # 1. 清理空白字符并去除末尾的斜杠（防止提取到空字符串）
    line = raw_text.strip().rstrip('/')
    if not line:
        return None
    
    # 2. 提取 URL 的最后一部分作为 username
    # split('/')[-1] 会拿到 "lunara_fawn"
    username = line.split('/')[-1]
    
    if not username:
        return None

    # 3. 构造文件名和执行指令
    target_file = f"{username}.json.gz"
    
    # 这里使用 Shell 的逻辑判断：
    # [ ! -f 文件名 ] 表示“如果文件不存在”
    # && 表示“则执行后面的命令”
    # 注意：这里假设你的运行环境是 Linux/macOS 或 Windows 的 Bash 环境
    translated_command = f'[ ! -f {target_file} ] && timeout 300s ~/twitter/venv/bin/python3 get2.py {username}'
    
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