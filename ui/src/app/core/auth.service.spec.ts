import { TestBed } from '@angular/core/testing';
import { provideHttpClient } from '@angular/common/http';
import { provideHttpClientTesting } from '@angular/common/http/testing';
import { AuthService } from './auth.service';

describe('AuthService', () => {
  it('starts without an authenticated principal', () => {
    TestBed.configureTestingModule({
      providers: [provideHttpClient(), provideHttpClientTesting()],
    });

    const auth = TestBed.inject(AuthService);

    expect(auth.principal()).toBeNull();
    expect(auth.busy()).toBeFalse();
  });
});
