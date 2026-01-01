cd server

go build .
~/script/scp.sh server root@cloudcone.moonchan.xyz:~/twitter
~/script/scp.sh ../.env root@cloudcone.moonchan.xyz:~/twitter

cd -