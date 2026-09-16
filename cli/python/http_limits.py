"""Bound bodies before JSON/multipart parsing, including chunked requests."""
import json


class BodyLimit:
    def __init__(self, app, limit):
        self.app, self.limit = app, limit

    async def __call__(self, scope, receive, send):
        if scope['type'] != 'http':
            return await self.app(scope, receive, send)
        chunks, size = [], 0
        while True:
            message = await receive()
            if message['type'] == 'http.disconnect':
                return
            size += len(message.get('body', b''))
            if size > self.limit:
                body = json.dumps({'error': {'message': 'request body too large', 'type': 'invalid_request_error'}}).encode()
                await send({'type': 'http.response.start', 'status': 413, 'headers': [(b'content-type', b'application/json')]})
                return await send({'type': 'http.response.body', 'body': body})
            chunks.append(message)
            if not message.get('more_body', False):
                break
        async def bounded_receive():
            if chunks:
                return chunks.pop(0)
            return await receive()
        await self.app(scope, bounded_receive, send)
