import { describe, expect, expectTypeOf, it } from 'vitest';
import {
  getAttachmentContent,
  getDocumentIndexStatus,
  getFileContent,
  runCLI,
  syncCLI
} from './generated/api/api';
import { deleteCLICollection } from './generated/cli/cli';
import { searchVisualAttachments } from './generated/exploration/exploration';

describe('generated request serialization', () => {
  it('sends each document media type as a separate query value', async () => {
    let query!: URLSearchParams;
    await getDocumentIndexStatus(
      {
        profile_id: 'test-profile',
        input_key: 'test-input',
        media_type: ['text/plain', 'application/pdf']
      },
      {
        fetch: async (url) => {
          query = new URL(String(url), 'https://example.invalid').searchParams;
          return Response.json({});
        }
      }
    );
    expect(query.getAll('media_type')).toEqual(['text/plain', 'application/pdf']);
    expect(query.get('profile_id')).toBe('test-profile');
  });

  it('preserves CLI folder selections, including literal commas', async () => {
    let query!: URLSearchParams;
    await syncCLI(
      { folder: ['Inbox', 'Archive,2026'], 'skip-folder': [] },
      {
        fetch: async (url) => {
          query = new URL(String(url), 'https://example.invalid').searchParams;
          return new Response('{"type":"complete"}\n');
        }
      }
    );
    expect(query.getAll('folder')).toEqual(['Inbox', 'Archive,2026']);
    expect(query.has('skip-folder')).toBe(false);
  });

  it('sends text visual searches as JSON', async () => {
    const body = { text: 'test image', limit: 5, directions: ['from_person'] };
    let outgoing!: Request;
    await searchVisualAttachments(body, {
      fetch: async (url, init) => {
        outgoing = new Request(new URL(String(url), 'https://example.invalid'), init);
        return Response.json({});
      }
    });
    expect(outgoing.headers.get('Content-Type')).toBe('application/json');
    expect(await outgoing.json()).toEqual(body);
  });

  it('sends image visual searches as multipart bytes and filter fields', async () => {
    const bytes = new Uint8Array([0, 255, 65]);
    let form!: FormData;
    let headers!: Headers;
    await searchVisualAttachments(
      {
        image: new Blob([bytes], { type: 'image/png' }),
        direction: ['from_person', 'to_person'],
        limit: '5',
        filename: 'test.png'
      },
      {
        fetch: async (_url, init) => {
          form = init!.body as FormData;
          headers = new Headers(init!.headers);
          return Response.json({});
        }
      }
    );
    expect(form).toBeInstanceOf(FormData);
    expect(headers.has('Content-Type')).toBe(false);
    const image = form.get('image');
    expect(image).toMatchObject({ size: bytes.length, type: 'image/png' });
    expect(form.getAll('direction')).toEqual(['from_person', 'to_person']);
    expect(form.get('limit')).toBe('5');
    expect(form.get('filename')).toBe('test.png');
    expect(form.has('cursor')).toBe(false);
  });
});

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
