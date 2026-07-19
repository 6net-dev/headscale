defmodule Registrar.Repo.Migrations.CreateCore do
  use Ecto.Migration

  def change do
    create table(:orgs) do
      add :slug, :string, null: false
      add :name, :string, null: false
      add :prefix_index, :integer, null: false

      timestamps(type: :utc_datetime_usec)
    end

    create unique_index(:orgs, [:slug])
    create unique_index(:orgs, [:prefix_index])

    create table(:devices) do
      add :name, :string, null: false
      add :node_key, :string, null: false

      timestamps(type: :utc_datetime_usec)
    end

    create unique_index(:devices, [:node_key])

    create table(:grants) do
      add :device_id, references(:devices, on_delete: :delete_all), null: false
      add :org_id, references(:orgs, on_delete: :delete_all), null: false
      add :identity, :string, null: false
      add :scope, :map, null: false, default: %{}
      add :host_index, :integer, null: false
      add :active, :boolean, null: false, default: false
      add :admission_expires_at, :utc_datetime_usec

      timestamps(type: :utc_datetime_usec)
    end

    create unique_index(:grants, [:device_id, :org_id])
    create unique_index(:grants, [:org_id, :host_index])
    create index(:grants, [:device_id])
  end
end
