import os
import unittest

import httpx

import gateway as G
from lsp2_server import LSP2Server

WEB = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "web")


class ChatUITest(unittest.IsolatedAsyncioTestCase):
    async def get(self, lsp, path):
        async with httpx.AsyncClient(transport=httpx.ASGITransport(app=lsp.asgi()), base_url="http://t") as c:
            return await c.get(path)

    async def test_served_with_csp(self):
        lsp = LSP2Server([], chat_ui_dir=WEB, log=lambda *a: None)
        for path, ctype in [("/", "text/html"), ("/chat", "text/html"), ("/chat/chat.js", "text/javascript"), ("/chat/qrcode.js", "text/javascript")]:
            r = await self.get(lsp, path)
            self.assertEqual(r.status_code, 200, path)
            self.assertTrue(r.headers["Content-Type"].startswith(ctype), path)
            self.assertEqual(r.headers["Content-Security-Policy"], G.WEB_CSP)

    async def test_disabled(self):
        self.assertEqual((await self.get(LSP2Server([], log=lambda *a: None), "/chat")).status_code, 404)


if __name__ == "__main__":
    unittest.main()
