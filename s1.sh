ulimit -n 1006500
sudo cp ./isucondition.conf /etc/nginx/sites-available/isucondition.conf
sudo cp ./sysctl.conf /etc/sysctl.conf
sudo sysctl -p
sudo systemctl restart nginx
sudo systemctl stop mysql
cd go
/home/isucon/local/go/bin/go build -o isucondition 
sudo systemctl restart isucondition.go.service
