ulimit -n 1006500
sudo cp ./sysctl.conf /etc/sysctl.conf
sudo sysctl -p
sudo systemctl stop nginx
sudo systemctl stop mysql
sudo systemctl stop isucondition.go.service
# cd go
#
# /home/isucon/local/go/bin/go build -o isucondition
# sudo systemctl restart isucondition.go.service
