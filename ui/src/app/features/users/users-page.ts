import { CommonModule } from '@angular/common';
import { Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule, NgForm } from '@angular/forms';
import { AuthService } from '../../core/auth.service';
import { AdminApiService } from '../../core/admin-api.service';
import { FeedbackService } from '../../core/feedback.service';
import { AdminUser } from '../../core/admin.types';
import { IconComponent } from '../../shared/icon/icon';
import { PageHeadingComponent } from '../../shared/page-heading/page-heading';

@Component({
  selector: 'app-users-page',
  standalone: true,
  imports: [CommonModule, FormsModule, IconComponent, PageHeadingComponent],
  templateUrl: './users-page.html',
})
export class UsersPageComponent implements OnInit {
  private readonly api = inject(AdminApiService);
  private readonly auth = inject(AuthService);
  private readonly feedback = inject(FeedbackService);
  protected readonly loading = signal(true);
  protected readonly busy = signal(false);
  protected readonly users = signal<AdminUser[]>([]);
  private readonly savedValues = new Map<string, { role: string; status: string }>();
  protected draft = { username: '', email: '', display_name: '', password: '', role: 'reader' };

  ngOnInit(): void {
    void this.load();
  }

  protected async load(): Promise<void> {
    this.loading.set(true);
    try {
      this.users.set(await this.api.listAll<AdminUser>('/api/v1/users'));
    } catch (error) {
      this.feedback.showError(AdminApiService.errorMessage(error, 'Unable to load users.'));
    } finally {
      this.loading.set(false);
    }
  }

  protected async create(form?: NgForm): Promise<void> {
    if (form && !form.valid) {
      form.form.markAllAsTouched();
      return;
    }
    this.busy.set(true);
    try {
      await this.api.post('/api/v1/users', this.draft);
      this.draft = { username: '', email: '', display_name: '', password: '', role: 'reader' };
      await this.load();
      this.feedback.showSuccess('User account created.');
    } catch (error) {
      this.feedback.showError(AdminApiService.errorMessage(error, 'Unable to create this user.'));
    } finally {
      this.busy.set(false);
    }
  }

  protected async save(user: AdminUser): Promise<void> {
    const previous = this.savedValues.get(user.id) ?? { role: user.role, status: user.status };
    this.busy.set(true);
    try {
      await this.api.patch(`/api/v1/users/${encodeURIComponent(user.id)}`, {
        role: user.role,
        status: user.status,
      });
      this.savedValues.delete(user.id);
      this.feedback.showSuccess(`Updated ${user.username}.`);
    } catch (error) {
      user.role = previous.role;
      user.status = previous.status;
      this.feedback.showError(AdminApiService.errorMessage(error, 'Unable to update this user.'));
    } finally {
      this.busy.set(false);
    }
  }

  protected setRole(user: AdminUser, role: string): void {
    this.savedValues.set(user.id, this.savedValues.get(user.id) ?? { role: user.role, status: user.status });
    user.role = role;
  }

  protected setStatus(user: AdminUser, status: string): void {
    this.savedValues.set(user.id, this.savedValues.get(user.id) ?? { role: user.role, status: user.status });
    user.status = status;
  }

  protected get currentUserId(): string {
    return this.auth.principal()?.id ?? '';
  }
}
