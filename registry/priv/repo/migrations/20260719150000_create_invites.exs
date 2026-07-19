defmodule Registrar.Repo.Migrations.CreateInvites do
  use Ecto.Migration

  def change do
    create table(:invites) do
      add :org_id, references(:orgs, on_delete: :delete_all), null: false
      add :token, :string, null: false
      add :identity, :string, null: false
      add :scope, :map, null: false, default: %{}
      add :expires_at, :utc_datetime_usec
      add :max_uses, :integer
      add :uses, :integer, null: false, default: 0

      timestamps(type: :utc_datetime_usec)
    end

    create unique_index(:invites, [:token])
    create index(:invites, [:org_id])
  end
end
