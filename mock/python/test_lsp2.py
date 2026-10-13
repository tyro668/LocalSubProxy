import asyncio
import json
import unittest

import lsp2 as L

KEY = L.SigningKey(bytes([7]) * 32)


class JWSTest(unittest.TestCase):
    def test_round_trip_and_rejections(self):
        jws = L.sign_jws(KEY, "at+jwt", KEY.kid, {"a": 1})
        self.assertEqual(L.verify_jws(KEY.pub, jws, "at+jwt"), b'{"a":1}')
        self.assertRaises(ValueError, L.verify_jws, KEY.pub, jws, "JWT")
        h, p, s = jws.split(".")
        self.assertRaises(ValueError, L.verify_jws, KEY.pub, h + "." + L.b64(b'{"a":2}') + "." + s, "at+jwt")
        self.assertRaises(ValueError, L.verify_jws, KEY.pub, f"{h}.{p}.{s}=", "at+jwt")
        crit = L.b64(b'{"alg":"EdDSA","typ":"x","crit":["b64"]}') + "." + p
        self.assertRaises(ValueError, L.verify_jws, KEY.pub, crit + "." + L.b64(KEY.priv.sign(crit.encode())), "x")

    def test_jwt_audience_and_lifetime(self):
        iat = L.now()
        tok = L.sign_jws(KEY, "at+jwt", "", {"aud": "a", "iat": iat, "exp": iat + 60})
        self.assertEqual(L.verify_jwt(KEY.pub, tok, "at+jwt", "a")["aud"], "a")
        self.assertRaises(ValueError, L.verify_jwt, KEY.pub, tok, "at+jwt", "b")
        self.assertRaises(ValueError, L.verify_jwt, KEY.pub, tok, "at+jwt", "a", iat + 200)

    def test_origin(self):
        self.assertEqual(L.origin("HTTPS://GW.Example.com:443/"), "https://gw.example.com")
        self.assertEqual(L.origin("http://127.0.0.1:8080"), "http://127.0.0.1:8080")
        self.assertRaises(ValueError, L.origin, "http://gw.example.com")
        self.assertRaises(ValueError, L.origin, "https://gw.example.com/path")


class FlowTest(unittest.IsolatedAsyncioTestCase):
    async def test_outbox(self):
        sent = []
        o = L.Outbox()
        o.set_flow({"task_window": 150, "conn_window": 1000})
        o.attach(lambda raw: sent.append(json.loads(raw)["seq"]))
        big = "x" * 80
        await o.send("t", lambda seq: {"type": "blob.chunk", "data": {"d": big}})
        second = asyncio.ensure_future(o.send("t", lambda seq: {"type": "blob.chunk", "data": {"d": big}}))
        await asyncio.sleep(0.01)
        self.assertFalse(second.done(), "second frame must wait for the window")
        o.ack("t", 1)
        await second
        self.assertEqual(sent, [1, 2])
        o.attach(lambda raw: sent.append("r%d" % json.loads(raw)["seq"]))  # reconnect: paused
        third = asyncio.ensure_future(o.send("t", lambda seq: {"type": "task.run", "jws": "a.b.c"}))
        await asyncio.sleep(0)
        o.ack("t", 2)
        await third
        self.assertEqual(sent, [1, 2])
        o.retransmit("t", 2)
        self.assertEqual(sent, [1, 2, "r3"])

    def test_inbound(self):
        i = L.Inbound({"task_window": 100, "conn_window": 1000})
        self.assertEqual(i.accept("t", 1, 60), L.ACCEPTED)
        self.assertEqual(i.accept("t", 1, 60), L.DUPLICATE)
        self.assertEqual(i.accept("t", 3, 60), L.GAP)
        self.assertEqual(i.accept("t", 2, 60), L.OVERFLOW)
        i.acked("t", 1)
        self.assertEqual(i.accept("t", 2, 60), L.ACCEPTED)
        self.assertEqual(i.recv("t"), 2)


if __name__ == "__main__":
    unittest.main()
