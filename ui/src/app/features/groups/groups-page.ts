import { CommonModule } from '@angular/common';
import { Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { AdminApiService } from '../../core/admin-api.service';
import { FeedbackService } from '../../core/feedback.service';
import { AdminGroup, GroupMember } from '../../core/admin.types';
import { IconComponent } from '../../shared/icon/icon';
import { PageHeadingComponent } from '../../shared/page-heading/page-heading';

@Component({
  selector: 'app-groups-page',
  standalone: true,
  imports: [CommonModule, FormsModule, IconComponent, PageHeadingComponent],
  templateUrl: './groups-page.html',
})
export class GroupsPageComponent implements OnInit {
  private readonly api = inject(AdminApiService);
  private readonly feedback = inject(FeedbackService);
  protected readonly loading = signal(true);
  protected readonly busy = signal(false);
  protected readonly groups = signal<AdminGroup[]>([]);
  protected readonly groupMembers = signal<GroupMember[]>([]);
  protected editingGroupId = '';
  protected draft = { name: '', members: '' };

  ngOnInit(): void {
    void this.load();
  }

  protected async load(): Promise<void> {
    this.loading.set(true);
    try {
      this.groups.set(await this.api.listAll<AdminGroup>('/api/v1/groups'));
    } catch (error) {
      this.feedback.showError(AdminApiService.errorMessage(error, 'Unable to load groups.'));
    } finally {
      this.loading.set(false);
    }
  }

  protected async edit(group: AdminGroup): Promise<void> {
    try {
      const result = await this.api.get<{ members: GroupMember[] }>(
        `/api/v1/groups/${encodeURIComponent(group.id)}`,
      );
      this.groupMembers.set(result.members);
      this.draft = {
        name: group.name,
        members: result.members.map((member) => member.id).join(', '),
      };
      this.editingGroupId = group.id;
      window.scrollTo({ top: 0, behavior: 'smooth' });
    } catch (error) {
      this.feedback.showError(AdminApiService.errorMessage(error, 'Unable to load group members.'));
    }
  }

  protected cancelEdit(): void {
    this.editingGroupId = '';
    this.groupMembers.set([]);
    this.draft = { name: '', members: '' };
  }

  protected async save(): Promise<void> {
    const name = this.draft.name.trim();
    const members = this.draft.members
      .split(',')
      .map((value) => value.trim())
      .filter(Boolean);
    this.busy.set(true);
    try {
      if (this.editingGroupId) {
        await this.api.patch(`/api/v1/groups/${encodeURIComponent(this.editingGroupId)}`, {
          name,
          members,
        });
        this.feedback.showSuccess('Group membership updated.');
      } else {
        await this.api.post('/api/v1/groups', { name, members });
        this.feedback.showSuccess('Group created.');
      }
      this.cancelEdit();
      await this.load();
    } catch (error) {
      this.feedback.showError(AdminApiService.errorMessage(error, 'Unable to save this group.'));
    } finally {
      this.busy.set(false);
    }
  }
}
