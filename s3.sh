ulimit -n 1006500
sudo cp ./sysctl.conf /etc/sysctl.conf
sudo sysctl -p
sudo systemctl stop nginx
sudo systemctl stop mysql
cd go
/home/isucon/local/go/bin/go build -o isucondition -gcflags='-l=4'
sudo systemctl restart isucondition.go.service
