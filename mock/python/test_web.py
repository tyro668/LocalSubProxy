import os
import unittest

from aiohttp.test_utils import AioHTTPTestCase

import gateway as G

WEB = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "web")


class ChatUITest(AioHTTPTestCase):
    async def get_application(self):
        return G.Gateway(web_dir=WEB, log=lambda *a: None).app()

    async def test_served_with_csp(self):
        for path, ctype in [("/", "text/html"), ("/chat", "text/html"), ("/chat/chat.js", "text/javascript"), ("/chat/qrcode.js", "text/javascript")]:
            r = await self.client.get(path)
            self.assertEqual(r.status, 200, path)
            self.assertTrue(r.headers["Content-Type"].startswith(ctype), path)
            self.assertEqual(r.headers["Content-Security-Policy"], G.WEB_CSP)


class ChatUIDisabledTest(AioHTTPTestCase):
    async def get_application(self):
        return G.Gateway(log=lambda *a: None).app()

    async def test_disabled(self):
        self.assertEqual((await self.client.get("/chat")).status, 404)


if __name__ == "__main__":
    unittest.main()
