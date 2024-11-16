cd go
go build -o isucondition
sudo systemctl restart isucondition.go.service
sudo rm /tmp/slow.log
sudo rm /var/log/nginx/access.log
sudo systemctl restart mysql
sudo systemctl restart nginx
