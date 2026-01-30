cd server

go build .
~/script/scp.sh server root@vps.moonchan.xyz:~/twitter/temp
~/script/scp.sh ../.env root@vps.moonchan.xyz:~/twitter
~/script/scp.sh ../get_meta_data.py root@vps.moonchan.xyz:~/twitter
~/script/scp.sh ../caller.py root@vps.moonchan.xyz:~/twitter
~/script/scp.sh ../bans.txt root@vps.moonchan.xyz:~/twitter

cd -

date;