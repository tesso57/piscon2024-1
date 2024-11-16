DB_USER=isucon
DB_PASS=isucon
DB_PORT=3306
DB_HOST=localhost

mysql -u$DB_USER -p$DB_PASS -h$DB_HOST -P$DB_PORT -e "set global slow_query_log_file = \"/tmp/slow.log\"; set global long_query_time = 0; set global slow_query_log = ON;"
sudo systemctl restart mysql
sudo systemctl restart isucondition.go
sudo systemctl restart nginx

