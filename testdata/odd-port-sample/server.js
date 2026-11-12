// Deliberately ignores $PORT and pins an unusual port: without AI, port
// detection has to guess (node -> 3000) and would guess wrong.
const http = require("http");

http
  .createServer((req, res) => {
    if (req.url === "/ping") {
      res.end("pong");
      return;
    }
    res.end("odd-port app, deployed by donkey");
  })
  .listen(5055, () => console.log("listening on 5055"));
