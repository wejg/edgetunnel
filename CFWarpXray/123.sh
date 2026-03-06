cd /Users/wei/edgetunnel/CFWarpXray

docker build -t cfwarpxray .

docker rm -f cfwarpxray
docker run -d --name cfwarpxray \
  --cap-add=NET_ADMIN --cap-add=NET_RAW --cap-add=MKNOD \
  --device-cgroup-rule 'c 10:200 rwm' \
  -p 16666:16666 -p 16667:16667 \
  cfwarpxray

  docker run -d --name cfwarpxray \
    --dns 1.1.1.1 \
    --dns 8.8.8.8 \
    --cap-add=NET_ADMIN --cap-add=NET_RAW --cap-add=MKNOD \
    --device-cgroup-rule 'c 10:200 rwm' \
    -p 16666:16666 -p 16667:16667 \
    cfwarpxray

cd /Users/diannao/edgetunnel/CFWarpXray
docker build -t cfwarpxray .
docker rm -f cfwarpxray 2>/dev/null
docker run -d --name cfwarpxray \
  --cap-add=NET_ADMIN --cap-add=NET_RAW --cap-add=MKNOD \
  --device-cgroup-rule 'c 10:200 rwm' \
  -p 16666:16666 -p 16667:16667 \
  cfwarpxray

  docker stop cfwarpxray
  docker rm cfwarpxray



  vless://a1b2c3d4-e5f6-7890-abcd-ef1234567890@127.0.0.1:16666?encryption=none#CFWarpXray