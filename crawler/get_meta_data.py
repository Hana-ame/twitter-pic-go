# get_meta_data.py 已合并收敛至主力脚本 get2.py（带 CT0 / 源 IP 绑定与完整的 users 表统计列更新）。
# 保留此文件仅作向后兼容转发入口。
from get2 import (
    auth_token,
    ct0_token,
    should_skip_processing,
    get_media_data_by_username,
    compute_media_counts,
    update_database,
    main,
)

if __name__ == "__main__":
    main()
