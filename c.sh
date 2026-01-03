cd server

go build .
~/script/scp.sh server root@cloudcone.moonchan.xyz:~/twitter/twitter.bin
~/script/scp.sh ../.env root@cloudcone.moonchan.xyz:~/twitter
~/script/scp.sh ../get_meta_data.py root@cloudcone.moonchan.xyz:~/twitter
~/script/scp.sh ../caller.py root@cloudcone.moonchan.xyz:~/twitter

cd -

date;