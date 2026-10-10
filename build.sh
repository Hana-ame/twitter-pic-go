cd server

go build .
~/script/scp.sh server root@vps.moonchan.xyz:~/twitter/temp
~/script/scp.sh ../.env root@vps.moonchan.xyz:~/twitter
~/script/scp.sh ../get_meta_data.py root@vps.moonchan.xyz:~/twitter
# get2.py 才是线上真正在跑的抓取脚本（带 CT0 / 源 IP 绑定）；漏它等于改了个寂寞
~/script/scp.sh ../get2.py root@vps.moonchan.xyz:~/twitter
~/script/scp.sh ../caller.py root@vps.moonchan.xyz:~/twitter
~/script/scp.sh ../bans.txt root@vps.moonchan.xyz:~/twitter

cd -

date;