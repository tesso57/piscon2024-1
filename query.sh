sudo pt-query-digest /tmp/slow.log > ~/log/$(date +mysql-slow.log-%m-%d-%H-%M -d "+9 hours")
sudo rm /tmp/slow.log
sudo systemctl restart mysql 
