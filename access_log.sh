sudo cat /var/log/nginx/access.log | alp ltsv -m"/api/isu/[a-f0-9\-]+","/api/isu/[a-f0-9\-]+/icon","/api/condition/[a-f0-9\-]+","/isu/[a-f0-9\-]+","/isu/[a-f0-9\-]+/graph","/isu/[a-f0-9\-]+/condition","/assets.*" --sort sum -r > ~/log/$(date +access.log-%m-%d-%H-%M -d "+9 hours")
sudo rm /var/log/nginx/access.log
sudo systemctl restart nginx
