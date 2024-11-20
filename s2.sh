ulimit -n 1006500
sudo cp ./sysctl.conf /etc/sysctl.conf
sudo sysctl -p
sudo systemctl stop nginx
sudo systemctl stop isucondition.go.service
sudo cp ./50-server.cnf /etc/mysql/mariadb.conf.d/50-server.cnf
sudo systemctl restart mysql
