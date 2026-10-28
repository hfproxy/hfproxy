import { provideHttpClient } from '@angular/common/http';
import { provideHttpClientTesting, HttpTestingController } from '@angular/common/http/testing';
import { TestBed } from '@angular/core/testing';
import { AdminApiService } from './admin-api.service';

describe('AdminApiService cursor pagination', () => {
  let api: AdminApiService;
  let http: HttpTestingController;

  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [provideHttpClient(), provideHttpClientTesting()],
    });
    api = TestBed.inject(AdminApiService);
    http = TestBed.inject(HttpTestingController);
  });

  afterEach(() => http.verify());

  async function flushMicrotasks(): Promise<void> {
    for (let i = 0; i < 10; i++) await Promise.resolve();
  }

  it('Issue88UI: follows every cursor before returning list items', async () => {
    const pending = api.listAll<{ id: string }>('/api/v1/groups');
    http.expectOne('/api/v1/groups?limit=200').flush({
      items: [{ id: 'first' }],
      next_cursor: 'opaque|cursor',
    });
    await flushMicrotasks();
    http.expectOne((request) => {
      const [path, query = ''] = request.urlWithParams.split('?');
      return path === '/api/v1/groups' && new URLSearchParams(query).get('cursor') === 'opaque|cursor';
    }).flush({
      items: [{ id: 'second' }],
      next_cursor: '',
    });
    expect(await pending).toEqual([{ id: 'first' }, { id: 'second' }]);
  });
});
