const http = require('http');

function createServer() {
  return http.createServer((req, res) => {
    if (req.url === '/health') {
      res.writeHead(200, { 'Content-Type': 'text/plain' });
      res.end('ok');
      return;
    }
    res.writeHead(200, { 'Content-Type': 'text/plain' });
    res.end('hello from harbor node\n');
  });
}

function startServer(port = process.env.PORT || 3000) {
  const server = createServer();
  server.listen(port, '127.0.0.1', () => {
    console.log(`hello-node listening on 127.0.0.1:${server.address().port}`);
  });
  return server;
}

if (require.main === module) {
  startServer();
}

module.exports = { createServer, startServer };
