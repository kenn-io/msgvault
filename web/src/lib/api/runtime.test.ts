import { describe, expect, expectTypeOf, it } from 'vitest';
import { getAttachmentContent, getFileContent, runCLI } from './generated/api/api';
import { deleteCLICollection } from './generated/cli/cli';

describe('generated response decoding', () => {
  it('keeps reserved characters inside a path parameter', async () => {
    let requestURL: RequestInfo | URL | undefined;
    await deleteCLICollection(
      { name: 'Team / Archive?' },
      {
        fetch: async (url) => {
          requestURL = url;
          return Response.json({ success: true });
        }
      }
    );
    expect(requestURL).toBe('/api/v1/cli/collections/Team%20%2F%20Archive%3F');
  });

  it.each(['application/octet-stream', 'application/json'])(
    'downloads attachment bytes as a Blob even with %s content',
    async (contentType) => {
      const bytes = new Uint8Array([0, 255, 65]);
      const { data } = await getAttachmentContent(
        { hash: 'a'.repeat(64) },
        {
          fetch: async () => new Response(bytes, { headers: { 'Content-Type': contentType } })
        }
      );

      expectTypeOf(data).toEqualTypeOf<Blob | undefined>();
      expect(data).toBeInstanceOf(Blob);
      expect(new Uint8Array(await data!.arrayBuffer())).toEqual(bytes);
    }
  );

  it('returns structured errors from a binary operation', async () => {
    const error = { error: 'not_found', message: 'Attachment not found' };
    const result = await getAttachmentContent(
      { hash: 'b'.repeat(64) },
      {
        fetch: async () => Response.json(error, { status: 404 })
      }
    );

    expect(result.data).toBeUndefined();
    expect(result.error).toEqual(error);
    expect(result.response.status).toBe(404);
  });

  it('exposes the original stream for bounded file previews', async () => {
    const response = new Response(new Uint8Array([0, 255, 65]));
    const result = await getFileContent({ id: 1 }, { fetch: async () => response });

    expectTypeOf(result.data).toEqualTypeOf<ReadableStream<Uint8Array> | undefined>();
    expect(result.data).toBe(response.body);
    expect(response.bodyUsed).toBe(false);
    await result.data!.cancel();
  });

  it('returns an event stream as text even when it contains only one event', async () => {
    const text = '{"type":"complete"}\n';
    const { data } = await runCLI(
      { args: ['embeddings', 'build'] },
      {
        fetch: async () =>
          new Response(text, {
            headers: { 'Content-Type': 'application/x-ndjson' }
          })
      }
    );

    expectTypeOf(data).toEqualTypeOf<string | undefined>();
    expect(data).toBe(text);
  });
});
